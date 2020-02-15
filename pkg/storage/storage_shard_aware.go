package storage

import (
	"context"

	beehivemetapb "github.com/deepfabric/beehive/pb/metapb"
	"github.com/deepfabric/busybee/pkg/pb/metapb"
	"github.com/fagongzi/log"
	"github.com/fagongzi/util/protoc"
)

const (
	becomeLeader = iota
	becomeFollower
)

type shardCycle struct {
	shard  beehivemetapb.Shard
	action int
}

func (h *beeStorage) Created(shard beehivemetapb.Shard) {

}

func (h *beeStorage) Splited(shard beehivemetapb.Shard) {

}

func (h *beeStorage) Destory(shard beehivemetapb.Shard) {

}

func (h *beeStorage) BecomeLeader(shard beehivemetapb.Shard) {
	if shard.Group == uint64(metapb.DefaultGroup) {
		h.shardC <- shardCycle{
			shard:  shard,
			action: becomeLeader,
		}
	}
}

func (h *beeStorage) BecomeFollower(shard beehivemetapb.Shard) {
	if shard.Group == uint64(metapb.DefaultGroup) {
		h.shardC <- shardCycle{
			shard:  shard,
			action: becomeFollower,
		}
	}
}

func (h *beeStorage) handleShardCycle(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			log.Infof("handle shard cycle task stopped")
			return
		case shard, ok := <-h.shardC:
			if ok {
				switch shard.action {
				case becomeLeader:
					h.doLoadEvent(shard.shard, true)
				case becomeFollower:
					h.doLoadEvent(shard.shard, false)
				}
			}
		}
	}
}

func (h *beeStorage) doLoadEvent(shard beehivemetapb.Shard, leader bool) {
	err := h.getStore(shard.ID).Scan(shard.Start, shard.End, func(key, value []byte) (bool, error) {
		if len(value) == 0 {
			return true, nil
		}

		switch value[0] {
		case instanceStartingType:
			if leader {
				instance := metapb.WorkflowInstance{}
				protoc.MustUnmarshal(&instance, value[1:])
				h.eventC <- Event{
					EventType: InstanceLoadedEvent,
					Data:      instance,
				}
			}
		case instanceStartedType:
			if leader {
				instance := metapb.WorkflowInstance{}
				protoc.MustUnmarshal(&instance, value[1:])
				h.eventC <- Event{
					EventType: InstanceStartedEvent,
					Data:      instance,
				}
			}
		case instanceStoppingType:
			if leader {
				instance := metapb.WorkflowInstance{}
				protoc.MustUnmarshal(&instance, value[1:])
				h.eventC <- Event{
					EventType: InstanceStoppingEvent,
					Data:      instance,
				}
			}
		case runningStateType:
			state := metapb.WorkflowInstanceState{}
			protoc.MustUnmarshal(&state, OriginInstanceStatePBValue(value[1:]))

			et := InstanceStateLoadedEvent
			if !leader {
				et = InstanceStateRemovedEvent
			}
			h.eventC <- Event{
				EventType: et,
				Data:      state,
			}
		}
		return true, nil
	}, false)
	if err != nil {
		log.Fatalf("scan shard data for loading failed with %+v", err)
	}
}
