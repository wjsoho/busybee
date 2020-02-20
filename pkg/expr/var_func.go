package expr

import (
	"fmt"
	"time"

	engine "github.com/fagongzi/expr"
	"github.com/fagongzi/log"
)

// func.name
type funcVar struct {
	valueType engine.VarType
	dynaFunc  func(Ctx) (interface{}, error)
}

func newFuncVar(name string, valueType engine.VarType) (engine.Expr, error) {
	expr := &funcVar{
		valueType: valueType,
	}

	switch name {
	case "year":
		expr.dynaFunc = yearFunc
	case "month":
		expr.dynaFunc = monthFunc
	case "day":
		expr.dynaFunc = dayFunc
	case "wf_step_crowd":
		expr.dynaFunc = stepCrowdFunc
	case "wf_step_ttl":
		expr.dynaFunc = stepTTLFunc
	default:
		return nil, fmt.Errorf("func %s not support", name)
	}

	return expr, nil
}

func (v *funcVar) Exec(data interface{}) (interface{}, error) {
	ctx, ok := data.(Ctx)
	if !ok {
		log.Fatalf("BUG: invalid expr ctx type %T", ctx)
	}

	value, err := v.dynaFunc(ctx)
	if err != nil {
		return nil, err
	}

	return convertByType(value, v.valueType)
}

func yearFunc(ctx Ctx) (interface{}, error) {
	return int64(time.Now().Year()), nil
}

func monthFunc(ctx Ctx) (interface{}, error) {
	return int64(time.Now().Month()), nil
}

func dayFunc(ctx Ctx) (interface{}, error) {
	return int64(time.Now().Day()), nil
}

func stepCrowdFunc(ctx Ctx) (interface{}, error) {
	return ctx.StepCrowd(), nil
}

func stepTTLFunc(ctx Ctx) (interface{}, error) {
	return ctx.StepTTL()
}
