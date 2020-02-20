package core

import (
	"fmt"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/deepfabric/busybee/pkg/notify"
	"github.com/deepfabric/busybee/pkg/pb/metapb"
	"github.com/deepfabric/busybee/pkg/pb/rpcpb"
	"github.com/deepfabric/busybee/pkg/storage"
	"github.com/deepfabric/busybee/pkg/util"
	"github.com/deepfabric/prophet"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/util/protoc"
	"github.com/stretchr/testify/assert"
)

func TestStart(t *testing.T) {
	store, deferFunc := storage.NewTestStorage(t, false)
	defer deferFunc()

	ng, err := NewEngine(store, notify.NewQueueBasedNotifier(store))
	assert.NoError(t, err, "TestStart failed")
	assert.NoError(t, ng.Start(), "TestStart failed")
}

func TestStop(t *testing.T) {
	store, deferFunc := storage.NewTestStorage(t, false)
	defer deferFunc()

	ng, err := NewEngine(store, notify.NewQueueBasedNotifier(store))
	assert.NoError(t, err, "TestStop failed")
	assert.NoError(t, ng.Start(), "TestStop failed")
	assert.NoError(t, ng.Stop(), "TestStop failed")
}

func TestCreateTenantQueue(t *testing.T) {
	store, deferFunc := storage.NewTestStorage(t, false)
	defer deferFunc()

	ng, err := NewEngine(store, notify.NewQueueBasedNotifier(store))
	assert.NoError(t, err, "TestCreateTenantQueue failed")
	assert.NoError(t, ng.Start(), "TestCreateTenantQueue failed")

	err = ng.CreateTenantQueue(10001, 1)
	assert.NoError(t, err, "TestCreateTenantQueue failed")

	time.Sleep(time.Millisecond * 500)

	c := 0
	err = store.RaftStore().Prophet().GetStore().LoadResources(16, func(res prophet.Resource) {
		c++
	})
	assert.Equal(t, 3, c, "TestCreateTenantQueue failed")
}

func TestStartInstance(t *testing.T) {
	store, deferFunc := storage.NewTestStorage(t, false)
	defer deferFunc()

	ng, err := NewEngine(store, notify.NewQueueBasedNotifier(store))
	assert.NoError(t, err, "TestStartInstance failed")
	assert.NoError(t, ng.Start(), "TestStartInstance failed")

	err = ng.CreateTenantQueue(10001, 1)
	assert.NoError(t, err, "TestStartInstance failed")
	time.Sleep(time.Second)

	bm := roaring.BitmapOf(1, 2, 3, 4)
	err = ng.StartInstance(metapb.Workflow{
		ID:       10000,
		TenantID: 10001,
		Duration: 10,
		Name:     "test_wf",
		Steps: []metapb.Step{
			metapb.Step{
				Name: "step_start",
				Execution: metapb.Execution{
					Type: metapb.Branch,
					Branches: []metapb.ConditionExecution{
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 1"),
							},
							NextStep: "step_end_1",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("1 == 1"),
							},
							NextStep: "step_end_else",
						},
					},
				},
			},
			metapb.Step{
				Name: "step_end_1",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_else",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
		},
	}, util.MustMarshalBM(bm), 3)
	assert.NoError(t, err, "TestStartInstance failed")

	time.Sleep(time.Second * 2)
	c := 0
	ng.(*engine).workers.Range(func(key, value interface{}) bool {
		c++
		return true
	})
	assert.Equal(t, 3, c, "TestStartInstance failed")

	err = ng.Storage().PutToQueue(10001, 0, metapb.TenantInputGroup, protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: 10001,
			UserID:   1,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("1"),
				},
			},
		},
	}))
	assert.NoError(t, err, "TestStartInstance failed")

	time.Sleep(time.Second)

	fetch := rpcpb.AcquireQueueFetchRequest()
	fetch.Key = storage.PartitionKey(10001, 0)
	fetch.CompletedOffset = 0
	fetch.Count = 1
	fetch.Consumer = []byte("c")
	data, err := ng.Storage().ExecCommandWithGroup(fetch, metapb.TenantOutputGroup)
	assert.NoError(t, err, "TestStartInstance failed")

	resp := rpcpb.AcquireBytesSliceResponse()
	protoc.MustUnmarshal(resp, data)
	assert.Equal(t, 1, len(resp.Values), "TestStartInstance failed")

	states, err := ng.InstanceCountState(10000)
	assert.NoError(t, err, "TestStartInstance failed")
	m := make(map[string]uint64)
	for _, state := range states.States {
		m[state.Step] = state.Count
	}
	assert.Equal(t, uint64(3), m["step_start"], "TestStartInstance failed")
	assert.Equal(t, uint64(1), m["step_end_1"], "TestStartInstance failed")
	assert.Equal(t, uint64(0), m["step_end_else"], "TestStartInstance failed")

	state, err := ng.InstanceStepState(10000, "step_start")
	assert.NoError(t, err, "TestStartInstance failed")
	bm = util.MustParseBM(state.Crowd)
	assert.Equal(t, uint64(3), bm.GetCardinality(), "TestStartInstance failed")

	time.Sleep(time.Second * 9)
	c = 0
	ng.(*engine).workers.Range(func(key, value interface{}) bool {
		c++
		return true
	})
	assert.Equal(t, 0, c, "TestStartInstance failed")
}

func TestUpdateCrowd(t *testing.T) {
	store, deferFunc := storage.NewTestStorage(t, false)
	defer deferFunc()

	tid := uint64(10001)
	wid := uint64(10000)

	ng, err := NewEngine(store, notify.NewQueueBasedNotifier(store))
	assert.NoError(t, err, "TestUpdateCrowd failed")
	assert.NoError(t, ng.Start(), "TestUpdateCrowd failed")

	err = ng.CreateTenantQueue(tid, 1)
	assert.NoError(t, err, "TestUpdateCrowd failed")
	time.Sleep(time.Second)

	bm := roaring.BitmapOf(2, 3, 4)
	err = ng.StartInstance(metapb.Workflow{
		ID:       wid,
		TenantID: tid,
		Name:     "test_wf",
		Steps: []metapb.Step{
			metapb.Step{
				Name: "step_start",
				Execution: metapb.Execution{
					Type: metapb.Branch,
					Branches: []metapb.ConditionExecution{
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 1"),
							},
							NextStep: "step_end_1",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("1 == 1"),
							},
							NextStep: "step_end_else",
						},
					},
				},
			},
			metapb.Step{
				Name: "step_end_1",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_else",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
		},
	}, util.MustMarshalBM(bm), 3)
	assert.NoError(t, err, "TestUpdateCrowd failed")

	time.Sleep(time.Second * 2)
	c := 0
	ng.(*engine).workers.Range(func(key, value interface{}) bool {
		c++
		return true
	})
	assert.Equal(t, 3, c, "TestUpdateCrowd failed")

	err = ng.Storage().PutToQueue(tid, 0, metapb.TenantInputGroup, protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: tid,
			UserID:   2,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("2"),
				},
			},
		},
	}), protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: tid,
			UserID:   3,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("3"),
				},
			},
		},
	}), protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: tid,
			UserID:   4,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("4"),
				},
			},
		},
	}))
	assert.NoError(t, err, "TestStartInstance failed")

	time.Sleep(time.Second * 2)

	states, err := ng.InstanceCountState(wid)
	assert.NoError(t, err, "TestUpdateCrowd failed")
	m := make(map[string]uint64)
	for _, state := range states.States {
		m[state.Step] = state.Count
	}
	assert.Equal(t, uint64(0), m["step_start"], "TestUpdateCrowd failed")
	assert.Equal(t, uint64(0), m["step_end_1"], "TestUpdateCrowd failed")
	assert.Equal(t, uint64(3), m["step_end_else"], "TestUpdateCrowd failed")

	err = ng.UpdateCrowd(wid, util.MustMarshalBM(roaring.BitmapOf(1, 2, 3, 5)))
	assert.NoError(t, err, "TestUpdateCrowd failed")

	err = ng.Storage().PutToQueue(tid, 0, metapb.TenantInputGroup, protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: tid,
			UserID:   1,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("1"),
				},
			},
		},
	}))
	assert.NoError(t, err, "TestUpdateCrowd failed")

	time.Sleep(time.Second * 2)

	states, err = ng.InstanceCountState(10000)
	assert.NoError(t, err, "TestUpdateCrowd failed")
	m = make(map[string]uint64)
	for _, state := range states.States {
		m[state.Step] = state.Count
	}
	assert.Equal(t, uint64(1), m["step_start"], "TestUpdateCrowd failed")
	assert.Equal(t, uint64(1), m["step_end_1"], "TestUpdateCrowd failed")
	assert.Equal(t, uint64(2), m["step_end_else"], "TestUpdateCrowd failed")
}

func TestUpdateWorkflow(t *testing.T) {
	store, deferFunc := storage.NewTestStorage(t, false)
	defer deferFunc()

	tid := uint64(10001)
	wid := uint64(10000)

	ng, err := NewEngine(store, notify.NewQueueBasedNotifier(store))
	assert.NoError(t, err, "TestUpdateWorkflow failed")
	assert.NoError(t, ng.Start(), "TestUpdateWorkflow failed")

	err = ng.CreateTenantQueue(tid, 1)
	assert.NoError(t, err, "TestUpdateWorkflow failed")
	time.Sleep(time.Second)

	bm := roaring.BitmapOf(1, 2, 3, 4)
	err = ng.StartInstance(metapb.Workflow{
		ID:       wid,
		TenantID: tid,
		Name:     "test_wf",
		Steps: []metapb.Step{
			metapb.Step{
				Name: "step_start",
				Execution: metapb.Execution{
					Type: metapb.Branch,
					Branches: []metapb.ConditionExecution{
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 1"),
							},
							NextStep: "step_end_1",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 2"),
							},
							NextStep: "step_end_2",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 3"),
							},
							NextStep: "step_end_3",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 4"),
							},
							NextStep: "step_end_4",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("1 == 1"),
							},
							NextStep: "step_end_else",
						},
					},
				},
			},
			metapb.Step{
				Name: "step_end_1",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_2",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_3",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_4",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_else",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
		},
	}, util.MustMarshalBM(bm), 3)
	assert.NoError(t, err, "TestUpdateWorkflow failed")

	time.Sleep(time.Second * 2)
	c := 0
	ng.(*engine).workers.Range(func(key, value interface{}) bool {
		c++
		return true
	})
	assert.Equal(t, 3, c, "TestUpdateWorkflow failed")

	err = ng.Storage().PutToQueue(tid, 0, metapb.TenantInputGroup, protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: tid,
			UserID:   1,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("1"),
				},
			},
		},
	}), protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: tid,
			UserID:   2,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("2"),
				},
			},
		},
	}), protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: tid,
			UserID:   3,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("3"),
				},
			},
		},
	}))
	assert.NoError(t, err, "TestUpdateWorkflow failed")

	time.Sleep(time.Second * 2)

	states, err := ng.InstanceCountState(wid)
	assert.NoError(t, err, "TestUpdateWorkflow failed")
	m := make(map[string]uint64)
	for _, state := range states.States {
		m[state.Step] = state.Count
	}
	assert.Equal(t, uint64(1), m["step_start"], "TestUpdateWorkflow failed")
	assert.Equal(t, uint64(1), m["step_end_1"], "TestUpdateWorkflow failed")
	assert.Equal(t, uint64(1), m["step_end_2"], "TestUpdateWorkflow failed")
	assert.Equal(t, uint64(1), m["step_end_3"], "TestUpdateWorkflow failed")
	assert.Equal(t, uint64(0), m["step_end_4"], "TestUpdateWorkflow failed")
	assert.Equal(t, uint64(0), m["step_end_else"], "TestUpdateWorkflow failed")

	err = ng.UpdateCrowd(wid, util.MustMarshalBM(roaring.BitmapOf(1, 2, 3, 5)))
	assert.NoError(t, err, "TestUpdateCrowd failed")

	err = ng.UpdateWorkflow(metapb.Workflow{
		ID:       wid,
		TenantID: tid,
		Name:     "test_wf",
		Steps: []metapb.Step{
			metapb.Step{
				Name: "step_start",
				Execution: metapb.Execution{
					Type: metapb.Branch,
					Branches: []metapb.ConditionExecution{
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 1"),
							},
							NextStep: "step_end_1",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 5"),
							},
							NextStep: "step_end_5",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 3"),
							},
							NextStep: "step_end_3",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 4"),
							},
							NextStep: "step_end_4",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("1 == 1"),
							},
							NextStep: "step_end_else",
						},
					},
				},
			},
			metapb.Step{
				Name: "step_end_1",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_5",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_3",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_4",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_else",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
		},
	})
	assert.NoError(t, err, "TestUpdateCrowd failed")

	time.Sleep(time.Second * 2)

	states, err = ng.InstanceCountState(wid)
	assert.NoError(t, err, "TestUpdateCrowd failed")
	m = make(map[string]uint64)
	for _, state := range states.States {
		m[state.Step] = state.Count
	}

	assert.Equal(t, uint64(1), m["step_start"], "TestUpdateWorkflow failed")
	assert.Equal(t, uint64(1), m["step_end_1"], "TestUpdateWorkflow failed")
	assert.Equal(t, uint64(0), m["step_end_5"], "TestUpdateWorkflow failed")
	assert.Equal(t, uint64(1), m["step_end_3"], "TestUpdateWorkflow failed")
	assert.Equal(t, uint64(0), m["step_end_4"], "TestUpdateWorkflow failed")
	assert.Equal(t, uint64(0), m["step_end_else"], "TestUpdateWorkflow failed")
}

func TestStartInstanceWithStepTTL(t *testing.T) {
	store, deferFunc := storage.NewTestStorage(t, false)
	defer deferFunc()

	ng, err := NewEngine(store, notify.NewQueueBasedNotifier(store))
	assert.NoError(t, err, "TestStartInstanceWithStepTTL failed")
	assert.NoError(t, ng.Start(), "TestStartInstanceWithStepTTL failed")

	err = ng.CreateTenantQueue(10001, 1)
	assert.NoError(t, err, "TestStartInstanceWithStepTTL failed")
	time.Sleep(time.Second)

	bm := roaring.BitmapOf(1, 2)
	err = ng.StartInstance(metapb.Workflow{
		ID:       10000,
		TenantID: 10001,
		Name:     "test_wf",
		Steps: []metapb.Step{
			metapb.Step{
				Name: "step_start",
				Execution: metapb.Execution{
					Type: metapb.Branch,
					Branches: []metapb.ConditionExecution{
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 1"),
							},
							NextStep: "step_ttl_start",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 2"),
							},
							NextStep: "step_ttl_start",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("1 == 1"),
							},
							NextStep: "step_end",
						},
					},
				},
			},
			metapb.Step{
				Name: "step_ttl_start",
				TTL:  2,
				Execution: metapb.Execution{
					Type: metapb.Branch,
					Branches: []metapb.ConditionExecution{
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: func.wf_step_ttl} > 0"),
							},
							NextStep: "step_ttl_end",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("1 == 1"),
							},
							NextStep: "step_end",
						},
					},
				},
			},
			metapb.Step{
				Name: "step_ttl_end",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
		},
	}, util.MustMarshalBM(bm), 3)
	assert.NoError(t, err, "TestStartInstanceWithStepTTL failed")

	time.Sleep(time.Second * 2)
	c := 0
	ng.(*engine).workers.Range(func(key, value interface{}) bool {
		c++
		return true
	})
	assert.Equal(t, 1, c, "TestStartInstanceWithStepTTL failed")

	err = ng.Storage().PutToQueue(10001, 0, metapb.TenantInputGroup, protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: 10001,
			UserID:   1,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("1"),
				},
			},
		},
	}), protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: 10001,
			UserID:   2,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("2"),
				},
			},
		},
	}))
	assert.NoError(t, err, "TestStartInstanceWithStepTTL failed")
	time.Sleep(time.Second)

	states, err := ng.InstanceCountState(10000)
	assert.NoError(t, err, "TestStartInstance failed")
	m := make(map[string]uint64)
	for _, state := range states.States {
		m[state.Step] = state.Count
	}
	assert.Equal(t, uint64(0), m["step_start"], "TestStartInstanceWithStepTTL failed")
	assert.Equal(t, uint64(2), m["step_ttl_start"], "TestStartInstanceWithStepTTL failed")
	assert.Equal(t, uint64(0), m["step_ttl_end"], "TestStartInstanceWithStepTTL failed")
	assert.Equal(t, uint64(0), m["ste_end"], "TestStartInstanceWithStepTTL failed")

	buf := goetty.NewByteBuf(24)
	v, err := store.Get(storage.WorkflowStepTTLKey(10000, 1, "step_ttl_start", buf))
	assert.NoError(t, err, "TestStartInstanceWithStepTTL failed")
	assert.NotEmpty(t, v, "TestStartInstanceWithStepTTL failed")

	v, err = store.Get(storage.WorkflowStepTTLKey(10000, 2, "step_ttl_start", buf))
	assert.NoError(t, err, "TestStartInstanceWithStepTTL failed")
	assert.NotEmpty(t, v, "TestStartInstanceWithStepTTL failed")

	err = ng.Storage().PutToQueue(10001, 0, metapb.TenantInputGroup, protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: 10001,
			UserID:   1,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("1"),
				},
			},
		},
	}))
	assert.NoError(t, err, "TestStartInstanceWithStepTTL failed")
	time.Sleep(time.Second * 2)
	err = ng.Storage().PutToQueue(10001, 0, metapb.TenantInputGroup, protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: 10001,
			UserID:   2,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("2"),
				},
			},
		},
	}))
	assert.NoError(t, err, "TestStartInstanceWithStepTTL failed")

	time.Sleep(time.Second)

	states, err = ng.InstanceCountState(10000)
	assert.NoError(t, err, "TestStartInstance failed")
	m = make(map[string]uint64)
	for _, state := range states.States {
		m[state.Step] = state.Count
	}
	assert.Equal(t, uint64(0), m["step_start"], "TestStartInstanceWithStepTTL failed")
	assert.Equal(t, uint64(0), m["step_ttl_start"], "TestStartInstanceWithStepTTL failed")
	assert.Equal(t, uint64(1), m["step_ttl_end"], "TestStartInstanceWithStepTTL failed")
	assert.Equal(t, uint64(1), m["step_end"], "TestStartInstanceWithStepTTL failed")
}

type errorNotify struct {
	times    int
	max      int
	delegate notify.Notifier
}

func newErrorNotify(max int, delegate notify.Notifier) notify.Notifier {
	return &errorNotify{
		max:      max,
		delegate: delegate,
	}
}

func (nt *errorNotify) Notify(id uint64, buf *goetty.ByteBuf, notifies ...metapb.Notify) error {
	if nt.times >= nt.max {
		return nt.delegate.Notify(id, buf, notifies...)
	}

	nt.times++
	return fmt.Errorf("error")
}

func TestStartInstanceWithNotifyError(t *testing.T) {
	store, deferFunc := storage.NewTestStorage(t, false)
	defer deferFunc()

	ng, err := NewEngine(store, newErrorNotify(1, notify.NewQueueBasedNotifier(store)))
	assert.NoError(t, err, "TestStartInstance failed")
	assert.NoError(t, ng.Start(), "TestStartInstance failed")

	err = ng.CreateTenantQueue(10001, 1)
	assert.NoError(t, err, "TestStartInstance failed")
	time.Sleep(time.Second)

	bm := roaring.BitmapOf(1, 2, 3, 4)
	err = ng.StartInstance(metapb.Workflow{
		ID:       10000,
		TenantID: 10001,
		Name:     "test_wf",
		Steps: []metapb.Step{
			metapb.Step{
				Name: "step_start",
				Execution: metapb.Execution{
					Type: metapb.Branch,
					Branches: []metapb.ConditionExecution{
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("{num: event.uid} == 1"),
							},
							NextStep: "step_end_1",
						},
						metapb.ConditionExecution{
							Condition: metapb.Expr{
								Value: []byte("1 == 1"),
							},
							NextStep: "step_end_else",
						},
					},
				},
			},
			metapb.Step{
				Name: "step_end_1",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
			metapb.Step{
				Name: "step_end_else",
				Execution: metapb.Execution{
					Type:   metapb.Direct,
					Direct: &metapb.DirectExecution{},
				},
			},
		},
	}, util.MustMarshalBM(bm), 3)
	assert.NoError(t, err, "TestStartInstance failed")

	time.Sleep(time.Second)
	c := 0
	ng.(*engine).workers.Range(func(key, value interface{}) bool {
		c++
		return true
	})
	assert.Equal(t, 3, c, "TestStartInstance failed")

	err = ng.Storage().PutToQueue(10001, 0, metapb.TenantInputGroup, protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: 10001,
			UserID:   1,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("1"),
				},
			},
		},
	}))
	assert.NoError(t, err, "TestStartInstance failed")

	time.Sleep(time.Second * 8)

	fetch := rpcpb.AcquireQueueFetchRequest()
	fetch.Key = storage.PartitionKey(10001, 0)
	fetch.CompletedOffset = 0
	fetch.Count = 1
	fetch.Consumer = []byte("c")
	data, err := ng.Storage().ExecCommandWithGroup(fetch, metapb.TenantOutputGroup)
	assert.NoError(t, err, "TestStartInstance failed")

	resp := rpcpb.AcquireBytesSliceResponse()
	protoc.MustUnmarshal(resp, data)
	assert.Equal(t, 1, len(resp.Values), "TestStartInstance failed")

	states, err := ng.InstanceCountState(10000)
	assert.NoError(t, err, "TestStartInstance failed")
	m := make(map[string]uint64)
	for _, state := range states.States {
		m[state.Step] = state.Count
	}
	assert.Equal(t, uint64(3), m["step_start"], "TestStartInstance failed")
	assert.Equal(t, uint64(1), m["step_end_1"], "TestStartInstance failed")
	assert.Equal(t, uint64(0), m["step_end_else"], "TestStartInstance failed")

	state, err := ng.InstanceStepState(10000, "step_start")
	assert.NoError(t, err, "TestStartInstance failed")
	bm = util.MustParseBM(state.Crowd)
	assert.Equal(t, uint64(3), bm.GetCardinality(), "TestStartInstance failed")
}
