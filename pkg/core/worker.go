package core

import (
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/deepfabric/beehive/util"
	"github.com/deepfabric/busybee/pkg/metric"
	"github.com/deepfabric/busybee/pkg/pb/metapb"
	"github.com/deepfabric/busybee/pkg/pb/rpcpb"
	"github.com/deepfabric/busybee/pkg/storage"
	bbutil "github.com/deepfabric/busybee/pkg/util"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/util/format"
	"github.com/fagongzi/util/protoc"
	"github.com/fagongzi/util/task"
	"github.com/robfig/cron/v3"
)

var (
	eventsCacheSize    = int64(4096)
	handleEventBatch   = int64(1024)
	maxTriggerCount    = 256
	ttlTriggerInterval = time.Second * 5
)

const (
	timerAction = iota
	userEventAction
	updateWorkflowAction
	updateCrowdAction
	checkTTLEventAction
	changeOffsetAction
)

type directCtx struct {
}

type item struct {
	action    int
	value     interface{}
	partition uint32
	offset    uint64
}

type triggerInfo struct {
	firstTS   int64
	alreadyBM *roaring.Bitmap
}

func (info *triggerInfo) maybeReset(ttl int32) {
	now := time.Now().Unix()
	if info.firstTS == 0 {
		info.firstTS = now
	}

	if now-info.firstTS > int64(ttl) {
		info.firstTS = now
		info.alreadyBM.Clear()
	}
}

type stateWorker struct {
	stopped                  uint32
	key                      string
	rkey                     string
	eng                      Engine
	wr                       *workerRunner
	buf                      *goetty.ByteBuf
	state                    metapb.WorkflowInstanceWorkerState
	totalCrowds              *roaring.Bitmap
	stepCrowds               []*roaring.Bitmap
	directNexts              map[string]string
	steps                    map[string]excution
	stepIndexs               map[string]int
	entryActions             map[string]string
	leaveActions             map[string]string
	alreadyTriggerTTLTimeout map[string]*triggerInfo
	queue                    *task.Queue
	items                    []interface{}
	cronIDs                  []cron.EntryID
	tenant                   string
	cond                     *rpcpb.Condition
	conditionKey             []byte
	queueStateKey            []byte
	queueGetStateKey         []byte

	tempBM       *roaring.Bitmap
	tempNotifies []metapb.Notify

	tran          *transaction
	offsets       map[uint32]uint64
	updateSuccess bool

	initState uint32

	lockKey, lockExpectValue []byte
}

func newStateWorker(key string,
	state metapb.WorkflowInstanceWorkerState,
	wr *workerRunner) (*stateWorker, error) {
	queueStateKey := make([]byte, 12, 12)
	goetty.Uint64ToBytesTo(state.InstanceID, queueStateKey)
	goetty.Uint32ToBytesTo(state.Index, queueStateKey[8:])

	conditionKey := make([]byte, 13, 13)
	copy(conditionKey, queueStateKey)
	conditionKey[12] = 0

	w := &stateWorker{
		key:              key,
		rkey:             runnerKey(&metapb.WorkerRunner{ID: state.TenantID, Index: state.Runner}),
		state:            state,
		eng:              wr.eng,
		wr:               wr,
		buf:              goetty.NewByteBuf(32),
		totalCrowds:      acquireBM(),
		queue:            task.New(eventsCacheSize),
		items:            make([]interface{}, handleEventBatch, handleEventBatch),
		tenant:           string(format.UInt64ToString(state.TenantID)),
		cond:             &rpcpb.Condition{},
		conditionKey:     conditionKey,
		queueStateKey:    queueStateKey,
		queueGetStateKey: storage.QueueKVKey(state.TenantID, queueStateKey),
		tempBM:           acquireBM(),
		tran:             newTransaction(),
		offsets:          make(map[uint32]uint64),
		lockKey:          wr.lockKey,
		lockExpectValue:  wr.lockExpectValue,
		updateSuccess:    true,
	}

	err := w.resetByState()
	if err != nil {
		metric.IncWorkflowWorkerFailed()
		return nil, err
	}

	return w, nil
}

func (w *stateWorker) resetTTLTimeout() {
	if w.alreadyTriggerTTLTimeout == nil {
		w.alreadyTriggerTTLTimeout = make(map[string]*triggerInfo)
	}

	for key, value := range w.alreadyTriggerTTLTimeout {
		delete(w.alreadyTriggerTTLTimeout, key)
		releaseBM(value.alreadyBM)
		value.alreadyBM = nil
	}
}

func (w *stateWorker) resetByState() error {
	w.totalCrowds.Clear()
	w.stepCrowds = w.stepCrowds[:0]
	w.directNexts = make(map[string]string)
	w.steps = make(map[string]excution)
	w.stepIndexs = make(map[string]int)
	w.entryActions = make(map[string]string)
	w.leaveActions = make(map[string]string)

	w.resetTTLTimeout()

	for idx, stepState := range w.state.States {
		bm := acquireBM()
		bbutil.MustParseBMTo(stepState.LoaderMeta, bm)
		w.stepCrowds = append(w.stepCrowds, bm)
		w.totalCrowds.Or(bm)

		exec, err := newExcution(stepState.Step.Name, stepState.Step.Execution)
		if err != nil {
			return err
		}

		if stepState.Step.Execution.Timer != nil {
			i := idx
			id, err := w.eng.AddCronJob(stepState.Step.Execution.Timer.Cron, func() {
				w.queue.Put(item{
					action: timerAction,
					value:  i,
				})
			})
			if err != nil {
				metric.IncWorkflowWorkerFailed()
				return err
			}

			w.cronIDs = append(w.cronIDs, id)
		}

		w.steps[stepState.Step.Name] = exec
		w.stepIndexs[stepState.Step.Name] = idx
		w.entryActions[stepState.Step.Name] = stepState.Step.EnterAction
		w.leaveActions[stepState.Step.Name] = stepState.Step.LeaveAction

		if stepState.Step.Execution.Type == metapb.Direct &&
			stepState.Step.Execution.Direct.NextStep != "" {
			w.directNexts[stepState.Step.Name] = stepState.Step.Execution.Direct.NextStep
		}

		if stepState.Step.TTL > 0 {
			w.alreadyTriggerTTLTimeout[stepState.Step.Name] = &triggerInfo{
				alreadyBM: acquireBM(),
			}
			err := w.checkTTLTimeoutLater(idx)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (w *stateWorker) stop() {
	atomic.StoreUint32(&w.stopped, 1)
}

func (w *stateWorker) workflowID() uint64 {
	return w.state.WorkflowID
}

func (w *stateWorker) cachedEventSize() int64 {
	return w.queue.Len()
}

func (w *stateWorker) isStopped() bool {
	return atomic.LoadUint32(&w.stopped) == 1
}

func (w *stateWorker) matches(uid uint32) bool {
	return w.totalCrowds.Contains(uid)
}

func (w *stateWorker) checkTTLTimeout(arg interface{}) {
	w.queue.Put(item{
		action: checkTTLEventAction,
		value:  arg,
	})
}

func (w *stateWorker) beloneTo(event *metapb.Event) bool {
	if w.totalCrowds.GetCardinality() == 0 {
		logger.Fatalf("%s %s bug 0 total", w.rkey, w.key)
	}

	switch event.Type {
	case metapb.UserType:
		if w.matches(event.User.UserID) {
			return true
		}
	case metapb.UpdateCrowdType:
		if event.UpdateCrowd.WorkflowID == w.state.WorkflowID &&
			event.UpdateCrowd.Index == w.state.Index {
			return true
		}
	case metapb.UpdateWorkflowType:
		if event.UpdateWorkflow.Workflow.ID == w.state.WorkflowID {
			return true
		}
	}

	return false
}

func (w *stateWorker) onEvent(p uint32, offset uint64, event *metapb.Event) (bool, error) {
	if w.queue.Len() >= eventsCacheSize {
		return false, nil
	}

	switch event.Type {
	case metapb.UserType:
		if event.User != nil && w.matches(event.User.UserID) {
			err := w.queue.Put(item{
				action:    userEventAction,
				value:     event.User,
				partition: p,
				offset:    offset,
			})

			if err != nil {
				return false, err
			}

			return true, nil
		}
	case metapb.UpdateCrowdType:
		if event.UpdateCrowd.WorkflowID == w.state.WorkflowID &&
			event.UpdateCrowd.Index == w.state.Index {
			err := w.queue.Put(item{
				action:    updateCrowdAction,
				value:     event.UpdateCrowd.Crowd,
				partition: p,
				offset:    offset,
			})
			if err != nil {
				return false, err
			}

			return true, nil
		}
	case metapb.UpdateWorkflowType:
		if event.UpdateWorkflow.Workflow.ID == w.state.WorkflowID {
			err := w.queue.Put(item{
				action:    updateWorkflowAction,
				value:     event.UpdateWorkflow.Workflow,
				partition: p,
				offset:    offset,
			})

			if err != nil {
				return false, err
			}

			return true, nil
		}
	}

	err := w.queue.Put(item{
		action:    changeOffsetAction,
		partition: p,
		offset:    offset,
	})
	if err != nil {
		return false, err
	}

	return true, nil
}

func (w *stateWorker) init(partitions uint32) {
	w.checkLastTranscation()

	logger.Infof("%s %s init with %d crowd, [%d, %d], version %d",
		w.rkey,
		w.key,
		w.totalCrowds.GetCardinality(),
		w.totalCrowds.Minimum(),
		w.totalCrowds.Maximum(),
		w.state.Version)
	atomic.CompareAndSwapUint32(&w.initState, 0, 1)
}

func (w *stateWorker) close() {
	logger.Infof("%s worker %s close", w.rkey, w.key)
	defer logger.Infof("%s worker %s close completed", w.rkey, w.key)

	w.tran.close()
	w.resetTTLTimeout()
	w.queue.Dispose()

	for _, id := range w.cronIDs {
		w.eng.StopCronJob(id)
	}

	for idx, bm := range w.stepCrowds {
		releaseBM(bm)
		w.stepCrowds[idx] = nil
	}

	releaseBM(w.tempBM)
	w.tempBM = nil
}

func (w *stateWorker) setOffset(p uint32, offset uint64) {
	if offset <= 0 {
		return
	}

	if v, ok := w.offsets[p]; ok && v >= offset {
		return
	}

	w.offsets[p] = offset
}

func (w *stateWorker) initCompleted() bool {
	return atomic.LoadUint32(&w.initState) == 1
}

func (w *stateWorker) handleEvent(completedCB func(string, uint32, uint64)) (bool, bool) {
	if w.queue.Len() == 0 && !w.queue.Disposed() {
		return false, true
	}

	w.buf.Clear()
	w.tran.start(w)

	changeOffsetCount := 0

	for {
		n, err := w.queue.Get(handleEventBatch, w.items)
		if err != nil {
			logger.Fatalf("BUG: queue can not disposed, but %+v", err)
		}

		for i := int64(0); i < n; i++ {
			value := w.items[i].(item)
			switch value.action {
			case timerAction:
				w.tran.doStepTimerEvent(value)
				w.completeTransaction(completedCB)
				if !w.updateSuccess {
					return true, false
				}
			case checkTTLEventAction:
				w.doCheckStepTTLTimeout(value.value.(int))
				w.completeTransaction(completedCB)
			case changeOffsetAction:
				changeOffsetCount++
				w.setOffset(value.partition, value.offset)
			case userEventAction:
				w.setOffset(value.partition, value.offset)
				w.tran.doStepUserEvent(value.value.(*metapb.UserEvent))
			case updateCrowdAction:
				w.flushUserEvent(completedCB)
				if !w.updateSuccess {
					return true, false
				}

				w.doUpdateCrowd(value.value.([]byte))
				if !w.updateSuccess {
					return true, false
				}

				completedCB(w.key, value.partition, value.offset)
				w.tran.start(w)
			case updateWorkflowAction:
				w.flushUserEvent(completedCB)
				if !w.updateSuccess {
					return true, false
				}

				w.doUpdateWorkflow(value.value.(metapb.Workflow))
				if !w.updateSuccess {
					return true, false
				}

				completedCB(w.key, value.partition, value.offset)
				w.tran.start(w)
			}
		}

		if w.queue.Len() == 0 ||
			int64(len(w.tran.userEvents)) >= handleEventBatch ||
			changeOffsetCount >= 1024 {
			break
		}
	}

	w.flushUserEvent(completedCB)
	return true, w.updateSuccess
}

func (w *stateWorker) flushUserEvent(completedCB func(string, uint32, uint64)) {
	if len(w.tran.userEvents) > 0 {
		w.tran.doStepFlushUserEvents()
	}

	w.completeTransaction(completedCB)
}

func (w *stateWorker) completeTransaction(completedCB func(string, uint32, uint64)) {
	hasChanged := len(w.tran.changes) > 0
	w.updateSuccess = true

	if !hasChanged {
		w.commitOffset(completedCB)
		return
	}

	for idx := range w.stepCrowds {
		w.stepCrowds[idx].Clear()
		w.stepCrowds[idx].Or(w.tran.stepCrowds[idx])
	}

	w.state.Version++
	w.retryDo("exec notify", w.tran, w.execNotify)
	w.retryDo("exec update state", w.tran, w.execUpdate)

	logger.Debugf("%s %s state update to version %d",
		w.rkey,
		w.key,
		w.state.Version)

	w.commitOffset(completedCB)
	w.tran.start(w)
}

func (w *stateWorker) commitOffset(completedCB func(string, uint32, uint64)) {
	for p, offset := range w.offsets {
		if w.updateSuccess {
			completedCB(w.key, p, offset)
			logger.Debugf("%s %s completed p/%d offset at %d",
				w.rkey,
				w.key,
				p,
				offset)
		}

		delete(w.offsets, p)
	}
}

func (w *stateWorker) checkLastTranscation() {
	// Every transcation will wirte a newest state to the store if crowd changed,
	// check the last version and the current, and do:
	// 1. last verison <= current version, transaction completed, remove transaction
	// 2. last verison > current version, last notify changed already added, but state was not added
	for {
		value, err := w.eng.Storage().GetWithGroup(w.queueGetStateKey, metapb.TenantOutputGroup)
		if err != nil {
			logger.Errorf("%s %s init load last transaction failed with %+v, retry later",
				w.rkey,
				w.key,
				err)
			time.Sleep(time.Second * 1)
			continue
		}

		if len(value) == 0 {
			logger.Infof("%s %s init has no last transcation",
				w.rkey,
				w.key)
			break
		}

		last := metapb.WorkflowInstanceWorkerState{}
		protoc.MustUnmarshal(&last, value)
		if last.Version <= w.state.Version {
			logger.Infof("%s %s init last transcation has been completed",
				w.rkey,
				w.key)
			break
		}

		logger.Infof("%s %s init update last transaction, start change version from %d to %d",
			w.rkey,
			w.key,
			w.state.Version,
			last.Version)

		w.state = last
		err = w.resetByState()
		if err != nil {
			logger.Fatalf("%s %s init reset state failed with %+v",
				w.rkey,
				w.key,
				err)
		}

		w.retryDo("update state by last transaction", nil, w.execUpdate)

		logger.Infof("%s %s init last transaction applied, version %d",
			w.rkey,
			w.key,
			w.state.Version)
		break
	}
}

func (w *stateWorker) execNotify(tran *transaction) error {
	totalMoved := uint64(0)
	w.tempNotifies = w.tempNotifies[:0]
	for _, changed := range tran.changes {
		nt := metapb.Notify{
			TenantID:       w.state.TenantID,
			WorkflowID:     w.state.WorkflowID,
			InstanceID:     w.state.InstanceID,
			UserID:         changed.user(),
			Crowd:          changed.crowd(),
			FromStep:       changed.from,
			ToStep:         changed.to,
			ToStepCycleTTL: changed.ttl,
			FromAction:     w.leaveActions[changed.from],
			ToAction:       w.entryActions[changed.to],
		}
		w.tempNotifies = append(w.tempNotifies, nt)
		totalMoved += changed.who.users.GetCardinality()

		logger.Debugf("%s %s moved %d to step %s",
			w.rkey,
			w.key,
			changed.who.users.GetCardinality(),
			changed.to)

		if logger.DebugEnabled() {
			iter := changed.who.users.Iterator()
			for {
				if !iter.HasNext() {
					break
				}

				logger.Debugf("%s %s move %d from %s to %s",
					w.rkey,
					w.key,
					iter.Next(),
					changed.from,
					changed.to)
			}
		}
	}

	w.buf.MarkWrite()
	w.buf.WriteUInt64(w.state.Version)
	condValue := w.buf.WrittenDataAfterMark()

	w.cond.Reset()
	w.cond.Key = w.conditionKey
	w.cond.Value = condValue.Data()
	w.cond.Cmp = rpcpb.LT
	err := w.eng.Notifier().Notify(w.state.TenantID, w.tempNotifies, w.cond,
		w.conditionKey, condValue.Data(),
		w.queueStateKey, protoc.MustMarshal(&w.state))
	if err != nil {
		return err
	}

	logger.Infof("%s %s moved %d",
		w.rkey,
		w.key,
		totalMoved)
	metric.IncUserMoved(totalMoved, w.tenant)
	return nil
}

func (w *stateWorker) execUpdate(batch *transaction) error {
	for idx := range w.state.States {
		w.state.States[idx].Loader = metapb.RawLoader
		w.state.States[idx].LoaderMeta = bbutil.MustMarshalBM(w.stepCrowds[idx])
		w.state.States[idx].TotalCrowd = w.stepCrowds[idx].GetCardinality()
	}

	req := rpcpb.AcquireUpdateInstanceStateShardRequest()
	req.State = w.state
	req.LockKey = w.lockKey
	req.LockExpectValue = w.lockExpectValue
	req.Ts = time.Now().Unix()

	logger.Debugf("%s %s ready update version %d",
		w.rkey,
		w.key,
		w.state.Version)
	value, err := w.eng.Storage().ExecCommandWithGroup(req, metapb.TenantRunnerGroup)
	if err != nil {
		logger.Errorf("%s %s ready update version %d completed with %+v, error %+v",
			w.rkey,
			w.key,
			w.state.Version,
			w.updateSuccess,
			err)
		return err
	}

	resp := rpcpb.AcquireBoolResponse()
	protoc.MustUnmarshal(resp, value)
	w.updateSuccess = resp.Value

	logger.Debugf("%s %s ready update version %d completed with %+v",
		w.rkey,
		w.key,
		w.state.Version,
		w.updateSuccess)
	return nil
}

func (w *stateWorker) doUpdateWorkflow(workflow metapb.Workflow) error {
	for _, id := range w.cronIDs {
		w.eng.StopCronJob(id)
	}
	w.cronIDs = w.cronIDs[:0]

	oldCrowds := make(map[string]*roaring.Bitmap)
	for idx, step := range w.state.States {
		oldCrowds[step.Step.Name] = w.stepCrowds[idx]
	}

	w.directNexts = make(map[string]string)
	w.steps = make(map[string]excution)
	w.stepIndexs = make(map[string]int)
	w.entryActions = make(map[string]string)
	w.leaveActions = make(map[string]string)

	w.resetTTLTimeout()

	var newCrowds []*roaring.Bitmap
	var newStates []metapb.StepState
	for idx, step := range workflow.Steps {
		exec, err := newExcution(step.Name, step.Execution)
		if err != nil {
			return err
		}

		if step.Execution.Timer != nil {
			i := idx
			id, err := w.eng.AddCronJob(step.Execution.Timer.Cron, func() {
				w.queue.Put(item{
					action: timerAction,
					value:  i,
				})
			})
			if err != nil {
				return err
			}

			w.cronIDs = append(w.cronIDs, id)
		}

		w.steps[step.Name] = exec
		w.stepIndexs[step.Name] = idx
		w.entryActions[step.Name] = step.EnterAction
		w.leaveActions[step.Name] = step.LeaveAction

		if step.Execution.Type == metapb.Direct &&
			step.Execution.Direct.NextStep != "" {
			w.directNexts[step.Name] = step.Execution.Direct.NextStep
		}

		newBM := acquireBM()
		if bm, ok := oldCrowds[step.Name]; ok {
			newBM.Or(bm)
		}

		newCrowds = append(newCrowds, newBM)
		newStates = append(newStates, metapb.StepState{
			Step: step,
		})

		if step.TTL > 0 {
			w.alreadyTriggerTTLTimeout[step.Name] = &triggerInfo{
				alreadyBM: acquireBM(),
			}

			err := w.checkTTLTimeoutLater(idx)
			if err != nil {
				return err
			}
		}
	}

	for idx, bm := range w.stepCrowds {
		releaseBM(bm)
		w.stepCrowds[idx] = nil
	}

	w.stepCrowds = newCrowds
	w.state.States = newStates
	w.state.Version++
	w.retryDo("exec update workflow", nil, w.execUpdate)
	logger.Debugf("worker %s workflow updated", w.key)
	return nil
}

func (w *stateWorker) doUpdateCrowd(crowd []byte) error {
	newTotal := acquireBM()
	defer releaseBM(newTotal)
	bbutil.MustParseBMTo(crowd, newTotal)

	newAdded := acquireBM()
	defer releaseBM(newAdded)
	newAdded.Or(newTotal)
	newAdded.AndNot(w.totalCrowds)

	w.totalCrowds.Clear()
	w.totalCrowds.Or(newTotal)

	for idx, sc := range w.stepCrowds {
		if idx == 0 {
			sc.Or(newAdded)
		}
		sc.And(newTotal)
	}

	w.state.Version++
	w.retryDo("exec update crowd", nil, w.execUpdate)
	logger.Infof("worker %s crowd updated", w.key)
	return nil
}

func (w *stateWorker) doCheckStepTTLTimeout(idx int) {
	if idx >= len(w.state.States) {
		return
	}

	if w.state.States[idx].Step.TTL <= 0 {
		return
	}

	defer w.checkTTLTimeoutLater(idx)

	currentBM := w.stepCrowds[idx]
	if currentBM.GetCardinality() == 0 {
		return
	}

	info := w.alreadyTriggerTTLTimeout[w.state.States[idx].Step.Name]
	if info == nil {
		logger.Fatalf("BUG: missing already trigger info")
	}

	info.maybeReset(w.state.States[idx].Step.TTL)
	alreadyBM := info.alreadyBM

	// current - (current and already)
	w.tempBM.Clear()
	w.tempBM.Or(currentBM)
	w.tempBM.AndNot(alreadyBM)

	if w.tempBM.GetCardinality() == 0 {
		return
	}

	count := 0
	itr := w.tempBM.Iterator()
	for {
		if !itr.HasNext() {
			break
		}

		value := itr.Next()
		alreadyBM.Add(value)
		w.tran.doStepUserEvent(&metapb.UserEvent{
			TenantID: w.state.TenantID,
			UserID:   value,
		})

		count++
		if count >= maxTriggerCount {
			break
		}
	}
}

func (w *stateWorker) isDirectStep(name string) bool {
	_, ok := w.directNexts[name]
	return ok
}

func (w *stateWorker) retryDo(thing string, tran *transaction, fn func(*transaction) error) {
	times := 1
	after := 2
	maxAfter := 30
	for {
		if w.wr.isStopped() {
			return
		}

		err := fn(tran)
		if err == nil {
			return
		}

		metric.IncStorageFailed()
		logger.Errorf("worker %s do %s failed %d times with %+v, retry after %d sec",
			w.key,
			thing,
			times,
			err,
			after)
		times++
		if after < maxAfter {
			after = after * 2
			if after > maxAfter {
				after = maxAfter
			}
		}
		time.Sleep(time.Second * time.Duration(after))
	}
}

func (w *stateWorker) checkTTLTimeoutLater(idx int) error {
	_, err := util.DefaultTimeoutWheel().Schedule(ttlTriggerInterval, w.checkTTLTimeout, idx)
	return err
}
