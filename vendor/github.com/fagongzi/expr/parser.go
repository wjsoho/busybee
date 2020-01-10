package expr

import (
	"fmt"

	"github.com/fagongzi/util/format"
)

const (
	tokenUnknown    = 10000
	tokenLeftParen  = 1 // (
	tokenRightParen = 2 // )
	tokenVarStart   = 3 // {
	tokenVarEnd     = 4 // }
	tokenLiteral    = 5 // "
	tokenCustom     = 100

	slash                    = '\\'
	quotation                = '"'
	quotationConversion byte = 0x00
	slashConversion     byte = 0x01
)

// CalcFunc a calc function returns a result
type CalcFunc func(interface{}, Expr, interface{}) (interface{}, error)

// Parser expr parser
type Parser interface {
	AddOP(string, CalcFunc)
	ValueType(...string)
	Parse([]byte) (Expr, error)
}

type parser struct {
	expr      *node
	stack     stack
	prevToken int
	lexer     Lexer
	template  *parserTemplate
}

type parserTemplate struct {
	startToken       int
	startConversion  byte
	opsTokens        map[int]string
	opsFunc          map[int]CalcFunc
	valueTypes       map[int]string
	defaultValueType string
	factory          VarExprFactory
}

// NewParser returns a expr parser
func NewParser(factory VarExprFactory) Parser {
	p := &parserTemplate{
		factory:    factory,
		opsTokens:  make(map[int]string),
		opsFunc:    make(map[int]CalcFunc),
		valueTypes: make(map[int]string),
		startToken: tokenCustom,
	}

	return p
}

func (p *parserTemplate) AddOP(op string, calcFunc CalcFunc) {
	p.startToken++
	p.opsTokens[p.startToken] = op
	p.opsFunc[p.startToken] = calcFunc
}

func (p *parserTemplate) ValueType(types ...string) {
	if len(types) == 0 {
		return
	}

	p.defaultValueType = types[0]
	for _, t := range types {
		p.startToken++
		p.valueTypes[p.startToken] = t
	}
}

func (p *parserTemplate) Parse(input []byte) (Expr, error) {
	return p.newParser(input).parse()
}

func (p *parserTemplate) newParser(input []byte) *parser {
	lexer := NewScanner(conversion(input))
	lexer.AddSymbol([]byte("("), tokenLeftParen)
	lexer.AddSymbol([]byte(")"), tokenRightParen)
	lexer.AddSymbol([]byte("{"), tokenVarStart)
	lexer.AddSymbol([]byte("}"), tokenVarEnd)
	lexer.AddSymbol([]byte("\""), tokenLiteral)

	for tokenValue, token := range p.opsTokens {
		lexer.AddSymbol([]byte(token), tokenValue)
	}

	for tokenValue, token := range p.valueTypes {
		lexer.AddSymbol([]byte(token), tokenValue)
	}

	return &parser{
		expr:      &node{},
		prevToken: tokenUnknown,
		template:  p,
		lexer:     lexer,
	}
}

func (p *parser) parse() (Expr, error) {
	p.stack.push(p.expr)
	for {
		p.lexer.NextToken()
		token := p.lexer.Token()

		var err error
		if token == tokenLeftParen {
			err = p.doLeftParen()
		} else if token == tokenRightParen {
			err = p.doRightParen()
		} else if token == tokenVarStart {
			err = p.doVarStart()
		} else if token == tokenLiteral {
			err = p.doLiteral()
		} else if token == tokenVarEnd {
			err = p.doVarEnd()
		} else if _, ok := p.template.opsTokens[token]; ok {
			err = p.doOp()
		} else if _, ok := p.template.valueTypes[token]; ok {
			err = p.doVarType()
		} else if token == TokenEOI {
			err = p.doEOI()
			if err != nil {
				return nil, err
			}

			return p.stack.pop(), nil
		}

		if err != nil {
			return nil, err
		}

		if token != tokenLiteral {
			p.prevToken = token
		}
	}
}

func (p *parser) doLeftParen() error {
	if p.prevToken == tokenUnknown { // (a+b)
		p.stack.append(&node{})
	} else if p.prevToken == tokenLeftParen { // ((a+b)*10)
		p.stack.append(&node{})
	} else if fn, ok := p.template.opsFunc[p.prevToken]; ok { // 10 * (a+b)
		p.stack.appendWithOP(fn, &node{})
	} else {
		return fmt.Errorf("unexpect token <%s> before %d",
			p.lexer.TokenSymbol(p.prevToken),
			p.lexer.TokenIndex())
	}

	p.lexer.SkipString()
	return nil
}

func (p *parser) doRightParen() error {
	var err error
	if p.prevToken == tokenRightParen || p.prevToken == tokenVarEnd { // (c + (a + b))
		p.stack.pop()
		p.lexer.SkipString()
	} else if fn, ok := p.template.opsFunc[p.prevToken]; ok { // (a + b)
		p.stack.current().appendWithOP(fn, newConstExpr(p.lexer.ScanString()))
		p.stack.pop()
	} else {
		return fmt.Errorf("unexpect token <%s> before %d",
			p.lexer.TokenSymbol(p.prevToken),
			p.lexer.TokenIndex())
	}

	return err
}

func (p *parser) doVarStart() error {
	if p.prevToken == tokenUnknown { // {
		p.stack.append(&node{})
	} else if p.prevToken == tokenLeftParen { // ({
		p.stack.append(&node{})
	} else if fn, ok := p.template.opsFunc[p.prevToken]; ok { // a + {
		p.stack.appendWithOP(fn, &node{})
	} else {
		return fmt.Errorf("unexpect token <%s> before %d",
			p.lexer.TokenSymbol(p.prevToken),
			p.lexer.TokenIndex())
	}

	p.lexer.SkipString()
	return nil
}

func (p *parser) doLiteral() error {
	if _, ok := p.template.opsFunc[p.prevToken]; ok { // a + "
		for {
			p.lexer.NextToken()
			if p.lexer.Token() == TokenEOI {
				return fmt.Errorf("missing \"")
			} else if p.lexer.Token() == tokenLiteral {
				break
			}
		}
	} else {
		return fmt.Errorf("unexpect token <%s> before %d",
			p.lexer.TokenSymbol(p.prevToken),
			p.lexer.TokenIndex())
	}

	return nil
}

func (p *parser) doVarEnd() error {
	varType := p.template.defaultValueType
	if p.prevToken == tokenVarStart { // {a}

	} else if t, ok := p.template.valueTypes[p.prevToken]; ok {
		varType = t
	} else {
		return fmt.Errorf("unexpect token <%s> before %d",
			p.lexer.TokenSymbol(p.prevToken),
			p.lexer.TokenIndex())
	}

	varExpr, err := p.template.factory(p.lexer.ScanString(), varType)
	if err != nil {
		return err
	}

	p.stack.current().append(varExpr)
	p.stack.pop()
	return nil
}

func (p *parser) doOp() error {
	var err error
	if p.prevToken == tokenUnknown { // 1 +
		p.stack.current().append(newConstExpr(p.lexer.ScanString()))
	} else if p.prevToken == tokenLeftParen { // (a+
		p.stack.current().append(newConstExpr(p.lexer.ScanString()))
	} else if p.prevToken == tokenRightParen { // (a+1) +
		p.lexer.SkipString()
	} else if fn, ok := p.template.opsFunc[p.prevToken]; ok { // a + b +
		p.stack.current().appendWithOP(fn, newConstExpr(p.lexer.ScanString()))
	} else if p.prevToken == tokenVarEnd { // {a} +
		p.lexer.SkipString()
	} else {
		return fmt.Errorf("unexpect token <%s> before %d",
			p.lexer.TokenSymbol(p.prevToken),
			p.lexer.TokenIndex())
	}

	return err
}

func (p *parser) doVarType() error {
	switch p.prevToken {
	case tokenVarStart:
	default:
		return fmt.Errorf("unexpect token <%s> before %d",
			p.lexer.TokenSymbol(p.prevToken),
			p.lexer.TokenIndex())
	}

	p.lexer.SkipString()
	return nil
}

func (p *parser) doEOI() error {
	if p.prevToken == tokenRightParen || p.prevToken == tokenVarEnd { // (a+b)

	} else if fn, ok := p.template.opsFunc[p.prevToken]; ok { // a + b
		p.stack.current().appendWithOP(fn, newConstExpr(p.lexer.ScanString()))
	} else {
		return fmt.Errorf("unexpect token <%s> before %d",
			p.lexer.TokenSymbol(p.prevToken),
			p.lexer.TokenIndex())
	}

	return nil
}

func newConstExpr(value []byte) Expr {
	if len(value) >= 2 && value[0] == quotation && value[len(value)-1] == quotation {
		return &constString{
			value: string(revertConversion(value[1 : len(value)-1])),
		}
	}

	strValue := string(value)
	int64Value, err := format.ParseStrInt64(strValue)
	if err != nil {
		return &constString{
			value: strValue,
		}
	}

	return &constInt64{
		value: int64Value,
	}
}

func revertConversion(src []byte) []byte {
	var dst []byte
	for _, v := range src {
		if v == slashConversion {
			dst = append(dst, slash)
		} else if v == quotationConversion {
			dst = append(dst, quotation)
		} else {
			dst = append(dst, v)
		}
	}

	return dst
}

func conversion(src []byte) []byte {
	// \" -> 0x00
	// \\ -> \
	var dst []byte
	for {
		if len(src) == 0 {
			return dst
		} else if len(src) == 1 {
			dst = append(dst, src...)
			return dst
		}

		if src[0] != slash {
			dst = append(dst, src[0])
			src = src[1:]
			continue
		}

		if src[0] == slash && src[1] == slash {
			dst = append(dst, slashConversion)
		} else if src[0] == slash && src[1] == quotation {
			dst = append(dst, quotationConversion)
		} else {
			dst = append(dst, src[0:2]...)
		}

		src = src[2:]
	}
}