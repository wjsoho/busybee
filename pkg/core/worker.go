package core

import (
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/deepfabric/beehive/util"
	"github.com/deepfabric/busybee/pkg/metric"
	"github.com/deepfabric/busybee/pkg/pb/metapb"
	"github.com/deepfabric/busybee/pkg/pb/rpcpb"
	"github.com/deepfabric/busybee/pkg/queue"
	"github.com/deepfabric/busybee/pkg/storage"
	bbutil "github.com/deepfabric/busybee/pkg/util"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/util/format"
	"github.com/fagongzi/util/protoc"
	"github.com/fagongzi/util/task"
	"github.com/robfig/cron/v3"
)

var (
	fetchEventBatch    = int64(2048)
	handleEventBatch   = int64(256)
	maxTriggerCount    = 256
	ttlTriggerInterval = time.Minute
)

const (
	stopAction = iota
	timerAction
	userEventAction
	updateCrowdEventAction
	updateWorkflowEventAction
	checkTTLEventAction
	initAction
)

type directCtx struct {
}

type item struct {
	action    int
	value     interface{}
	cb        *stepCB
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
	eng                      Engine
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
	cronIDs                  []cron.EntryID
	consumer                 queue.AsyncConsumer
	tenant                   string
	cond                     *rpcpb.Condition
	conditionKey             []byte
	queueStateKey            []byte
	queueGetStateKey         []byte

	tempBM       *roaring.Bitmap
	tempEvents   []metapb.UserEvent
	tempNotifies []metapb.Notify

	items []interface{}
	tran  *transaction
}

func newStateWorker(key string, state metapb.WorkflowInstanceWorkerState, eng Engine) (*stateWorker, error) {
	consumer, err := queue.NewAsyncConsumer(state.TenantID, eng.Storage(), []byte(key))
	if err != nil {
		metric.IncStorageFailed()
		return nil, err
	}

	queueStateKey := make([]byte, 12, 12)
	goetty.Uint64ToBytesTo(state.InstanceID, queueStateKey)
	goetty.Uint32ToBytesTo(state.Index, queueStateKey[8:])

	conditionKey := make([]byte, 13, 13)
	copy(conditionKey, queueStateKey)
	conditionKey[12] = 0

	w := &stateWorker{
		key:              key,
		state:            state,
		eng:              eng,
		buf:              goetty.NewByteBuf(32),
		totalCrowds:      acquireBM(),
		queue:            task.New(1024),
		consumer:         consumer,
		tenant:           string(format.UInt64ToString(state.TenantID)),
		cond:             &rpcpb.Condition{},
		conditionKey:     conditionKey,
		queueStateKey:    queueStateKey,
		queueGetStateKey: storage.QueueKVKey(state.TenantID, queueStateKey),
		tempBM:           acquireBM(),
		items:            make([]interface{}, handleEventBatch, handleEventBatch),
		tran:             newTransaction(),
	}

	err = w.resetByState()
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
	w.queue.Put(item{
		action: stopAction,
	})
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

func (w *stateWorker) onFilterEvent(item []byte) (interface{}, bool) {
	event := metapb.Event{}
	protoc.MustUnmarshal(&event, item)

	switch event.Type {
	case metapb.UserType:
		if w.matches(event.User.UserID) {
			return event, true
		}
		return metapb.Event{}, false
	case metapb.UpdateCrowdType:
		if event.UpdateCrowd.WorkflowID == w.state.WorkflowID &&
			event.UpdateCrowd.Index == w.state.Index {
			return event, true
		}
		return metapb.Event{}, false
	case metapb.UpdateWorkflowType:
		if event.UpdateWorkflow.Workflow.ID == w.state.WorkflowID {
			return event, true
		}
		return metapb.Event{}, false
	}

	return metapb.Event{}, false
}

func (w *stateWorker) onEvent(partition uint32, items []interface{}) (int, error) {
	logger.Debugf("worker %s consumer %d items from queue partition %d",
		w.key,
		len(items),
		partition)

	completed := -1
	var events []metapb.UserEvent
	for idx, item := range items {
		event := item.(metapb.Event)
		logger.Debugf("worker %s consumer event %+v",
			w.key,
			event)

		switch event.Type {
		case metapb.UserType:
			if w.matches(event.User.UserID) {
				evt := *event.User
				evt.WorkflowID = w.state.WorkflowID
				evt.InstanceID = w.state.InstanceID
				events = append(events, evt)
			}
		case metapb.UpdateCrowdType:
			if event.UpdateCrowd.WorkflowID == w.state.WorkflowID &&
				event.UpdateCrowd.Index == w.state.Index {
				if len(events) > 0 {
					err := w.doEvent(userEventAction, events)
					if err != nil {
						return completed, err
					}

					completed = idx - 1
					events = events[:0]
				}

				err := w.doEvent(updateCrowdEventAction, event.UpdateCrowd.Crowd)
				if err != nil {
					return completed, err
				}
				completed = idx
			}
		case metapb.UpdateWorkflowType:
			if event.UpdateWorkflow.Workflow.ID == w.state.WorkflowID {
				if len(events) > 0 {
					err := w.doEvent(userEventAction, events)
					if err != nil {
						return completed, err
					}

					completed = idx - 1
					events = events[:0]
				}

				err := w.doEvent(updateWorkflowEventAction, event.UpdateWorkflow.Workflow)
				if err != nil {
					return completed, err
				}
				completed = idx
			}
		}
	}

	if len(events) > 0 {
		err := w.doEvent(userEventAction, events)
		if err != nil {
			return completed, err
		}
	}

	if len(items) > 0 {
		metric.IncEventHandled(len(items), w.tenant, metapb.TenantInputGroup)
	}

	return 0, nil
}

func (w *stateWorker) doEvent(action int, data interface{}) error {
	cb := acquireCB()
	defer releaseCB(cb)

	err := w.queue.Put(item{
		action: action,
		value:  data,
		cb:     cb,
	})
	if err != nil {
		return err
	}

	return cb.wait()
}

func (w *stateWorker) init() {
	w.queue.Put(item{
		action: initAction,
	})
}

func (w *stateWorker) handleEvent() bool {
	if w.queue.Len() == 0 && !w.queue.Disposed() {
		return false
	}

	w.buf.Clear()
	n, err := w.queue.Get(handleEventBatch, w.items)
	if err != nil {
		logger.Fatalf("BUG: fetch from work queue failed with %+v", err)
	}

	w.tran.start(w)
	for i := int64(0); i < n; i++ {
		value := w.items[i].(item)
		if value.action == stopAction {
			w.tran.close()
			w.consumer.Stop()
			w.resetTTLTimeout()

			for _, v := range w.queue.Dispose() {
				cb := (v.(item).cb)
				cb.complete(task.ErrDisposed)
				cb.reset()
			}

			for _, id := range w.cronIDs {
				w.eng.StopCronJob(id)
			}

			for _, bm := range w.stepCrowds {
				releaseBM(bm)
			}
			releaseBM(w.totalCrowds)

			releaseBM(w.tempBM)
			logger.Infof("worker %s stopped", w.key)
			return false
		}

		switch value.action {
		case initAction:
			w.checkLastTranscation()
			w.consumer.Start(uint64(fetchEventBatch), w.onFilterEvent, w.onEvent)
			logger.Infof("worker %s init with %d crowd, [%d, %d]",
				w.key,
				w.totalCrowds.GetCardinality(),
				w.totalCrowds.Minimum(),
				w.totalCrowds.Maximum())
		case timerAction:
			w.tran.doStepTimerEvent(value)
		case userEventAction:
			w.tran.doStepUserEvents(value)
		case updateCrowdEventAction:
			err = w.tran.err
			w.completeTransaction()
			if err != nil {
				value.cb.complete(err)
			} else {
				value.cb.complete(w.doUpdateCrowd(value.value.([]byte)))
			}

			w.tran.start(w)
		case updateWorkflowEventAction:
			err = w.tran.err
			w.completeTransaction()
			if err != nil {
				value.cb.complete(err)
			} else {
				value.cb.complete(w.doUpdateWorkflow(value.value.(metapb.Workflow)))
			}

			w.tran.start(w)
		case checkTTLEventAction:
			w.doCheckStepTTLTimeout(value.value.(int))
		}

		if err != nil {
			break
		}
	}

	w.completeTransaction()
	return true
}

func (w *stateWorker) completeTransaction() {
	defer w.tran.reset()

	if w.tran.err != nil {
		for _, c := range w.tran.cbs {
			c.complete(w.tran.err)
		}
		return
	}

	if len(w.tran.changes) > 0 {
		for idx := range w.stepCrowds {
			w.stepCrowds[idx].Clear()
			w.stepCrowds[idx].Or(w.tran.stepCrowds[idx])
		}

		w.state.Version++
		w.retryDo("exec notify", w.tran, w.execNotify)
		w.retryDo("exec update state", w.tran, w.execUpdate)

		logger.Debugf("worker %s state update to version %d",
			w.key,
			w.state.Version)
	}

	for _, c := range w.tran.cbs {
		c.complete(nil)
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
			logger.Errorf("worker %s load last transaction failed with %+v, retry later",
				w.key,
				err)
			time.Sleep(time.Second * 1)
			continue
		}

		if len(value) == 0 {
			logger.Infof("worker %s has no last transcation",
				w.key)
			break
		}

		last := metapb.WorkflowInstanceWorkerState{}
		protoc.MustUnmarshal(&last, value)
		if last.Version <= w.state.Version {
			break
		}

		w.state = last
		err = w.resetByState()
		if err != nil {
			logger.Fatalf("worker %s reset state failed with %+v",
				w.key, err)
		}

		w.retryDo("update state by last trnasaction", nil, w.execUpdate)
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

		if logger.DebugEnabled() {
			iter := changed.who.users.Iterator()
			for {
				if !iter.HasNext() {
					break
				}

				logger.Debugf("worker %s move %d from %s to %s",
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
	w.cond.Value = condValue
	w.cond.Cmp = rpcpb.LT
	err := w.eng.Notifier().Notify(w.state.TenantID, w.buf, w.tempNotifies, w.cond,
		w.conditionKey, condValue,
		w.queueStateKey, protoc.MustMarshal(&w.state))
	if err != nil {
		return err
	}

	logger.Infof("worker %s moved %d",
		w.key,
		totalMoved)

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
	_, err := w.eng.Storage().ExecCommandWithGroup(req, metapb.TenantRunnerGroup)
	return err
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

	for _, bm := range w.stepCrowds {
		releaseBM(bm)
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
	if w.tran.err != nil {
		return
	}

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
	w.tempEvents = w.tempEvents[:0]
	for {
		if !itr.HasNext() {
			break
		}

		value := itr.Next()
		alreadyBM.Add(value)
		w.tempEvents = append(w.tempEvents, metapb.UserEvent{
			TenantID:   w.state.TenantID,
			WorkflowID: w.state.WorkflowID,
			InstanceID: w.state.InstanceID,
			UserID:     value,
		})

		count++
		if count >= maxTriggerCount {
			break
		}
	}

	w.tran.doUserEvents(w.tempEvents)
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
		if w.isStopped() {
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
