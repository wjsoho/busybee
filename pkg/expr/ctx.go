package expr

import (
	"github.com/deepfabric/busybee/pkg/pb/metapb"
)

// Ctx expr excution ctx
type Ctx interface {
	Event() metapb.UserEvent
	Profile([]byte) ([]byte, error)
	KV([]byte) ([]byte, error)
}
