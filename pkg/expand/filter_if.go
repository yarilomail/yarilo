package expand

import (
	"errors"
	"fmt"
	"regexp"
)

type compareOp int

const (
	opUnknown compareOp = iota
	opNumEq
	opNumLt
	opNumLe
	opNumGt
	opNumGe
	opNumNe
	opStrEq // every numeric comparison sorts before this one
	opStrLt
	opStrLe
	opStrGt
	opStrGe
	opStrNe
	opLike
	opNotLike
	opRegexp
	opNotRegexp
)

var compareOps = map[string]compareOp{
	"==": opNumEq, "<": opNumLt, "<=": opNumLe, ">": opNumGt, ">=": opNumGe, "!=": opNumNe,
	"eq": opStrEq, "lt": opStrLt, "le": opStrLe, "gt": opStrGt, "ge": opStrGe, "ne": opStrNe,
	"*": opLike, "!*": opNotLike, "~": opRegexp, "!~": opNotRegexp,
}

func (c *call) compare(lhs *param, op compareOp, rhs *param) (bool, error) {
	if op < opStrEq {
		a, err := c.number(lhs)
		if err != nil {
			return false, fmt.Errorf("Left-hand side: %w", err)
		}
		b, err := c.number(rhs)
		if err != nil {
			return false, fmt.Errorf("Right-hand side: %w", err)
		}
		switch op {
		case opNumEq:
			return a == b, nil
		case opNumLt:
			return a < b, nil
		case opNumLe:
			return a <= b, nil
		case opNumGt:
			return a > b, nil
		case opNumGe:
			return a >= b, nil
		default:
			return a != b, nil
		}
	}
	a, err := c.str(lhs)
	if err != nil {
		return false, fmt.Errorf("Left-hand side %w", err)
	}
	b, err := c.str(rhs)
	if err != nil {
		return false, fmt.Errorf("Right-hand side %w", err)
	}
	switch op {
	case opStrEq:
		return a == b, nil
	case opStrLt:
		return a < b, nil
	case opStrLe:
		return a <= b, nil
	case opStrGt:
		return a > b, nil
	case opStrGe:
		return a >= b, nil
	case opStrNe:
		return a != b, nil
	case opLike:
		return wildcardMatch(a, b), nil
	case opNotLike:
		return !wildcardMatch(a, b), nil
	}
	re, err := regexp.Compile(b)
	if err != nil {
		return false, err
	}
	matched := firstNonEmptyMatch(re, a) != nil
	return matched != (op == opNotRegexp), nil
}

// wildcardMatch matches data against a mask where * is any run and ? any one
// character.
func wildcardMatch(data, mask string) bool {
	if mask == "" {
		return data == ""
	}
	switch mask[0] {
	case '*':
		for i := 0; i <= len(data); i++ {
			if wildcardMatch(data[i:], mask[1:]) {
				return true
			}
		}
		return false
	case '?':
		return data != "" && wildcardMatch(data[1:], mask[1:])
	}
	return data != "" && data[0] == mask[0] && wildcardMatch(data[1:], mask[1:])
}

func fnIf(c *call) error {
	firstIsLHS := !c.st.set
	var lhs, rhs, onTrue, onFalse *param
	var opName string
	step := 0
	if !firstIsLHS {
		step = 1
	}
	for i := range c.params() {
		p := &c.params()[i]
		if p.key != "" {
			return errUnsupportedKey(p.key)
		}
		switch step {
		case 0:
			lhs = p
		case 1:
			v, err := c.str(p)
			if err != nil {
				return fmt.Errorf("Comparator: %w", err)
			}
			opName = v
		case 2:
			rhs = p
		case 3:
			onTrue = p
		case 4:
			onFalse = p
		default:
			return errTooManyPositional
		}
		step++
	}
	if step != 5 {
		return errors.New("Missing parameters")
	}
	op := compareOps[opName]
	if op == opUnknown {
		return fmt.Errorf("Unsupported comparator '%s'", opName)
	}
	if !firstIsLHS {
		p, ok := c.fromTransfer(op < opStrEq)
		if !ok {
			if op < opStrEq {
				return errors.New("Input is not a number")
			}
			return errors.New("No value to use as left-hand in if")
		}
		lhs = p
	}
	result, err := c.compare(lhs, op, rhs)
	if err != nil {
		return err
	}
	branch, name := onFalse, "False branch"
	if result {
		branch, name = onTrue, "True branch"
	}
	v, err := c.any(branch)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	c.st.setString(v)
	return nil
}

type switchCase struct{ cond, value *param }

// fnSwitch is switch(lhs, op, cond, value, ..., default); lhs may be the input
// and the default may be left out.
func fnSwitch(c *call) error {
	firstIsLHS := !c.st.set
	var lhs, cond *param
	var opName string
	var cases []switchCase
	const (
		stLHS = iota
		stOp
		stCond
		stValue
	)
	step := stLHS
	if !firstIsLHS {
		step = stOp
	}
	for i := range c.params() {
		p := &c.params()[i]
		if p.key != "" {
			return errUnsupportedKey(p.key)
		}
		switch step {
		case stLHS:
			lhs, step = p, stOp
		case stOp:
			v, err := c.str(p)
			if err != nil {
				return fmt.Errorf("Comparator: %w", err)
			}
			opName, step = v, stCond
		case stCond:
			cond, step = p, stValue
		case stValue:
			cases = append(cases, switchCase{cond: cond, value: p})
			cond, step = nil, stCond
		}
	}
	switch {
	case step > stOp && len(cases) == 0:
		return errors.New("At least one condition-value pair is required")
	case step == stValue && cond != nil:
		cases = append(cases, switchCase{value: cond})
	case step == stCond:
		cases = append(cases, switchCase{})
	default:
		return errors.New("Missing parameters")
	}
	op := compareOps[opName]
	if op == opUnknown {
		return fmt.Errorf("Unsupported comparator '%s'", opName)
	}
	if !firstIsLHS {
		p, ok := c.fromTransfer(op < opStrEq)
		if !ok {
			if op < opStrEq {
				return errors.New("Input is not a number")
			}
			return errors.New("No value to use as left-hand in switch")
		}
		lhs = p
	}
	var res *param
	n := 0
	for _, sc := range cases {
		if sc.cond == nil {
			res = sc.value
			break
		}
		match, err := c.compare(lhs, op, sc.cond)
		if err != nil {
			return err
		}
		if match {
			res = sc.value
			break
		}
		n++
	}
	if res == nil {
		c.st.unset()
		return errors.New("No default value provided")
	}
	v, err := c.any(res)
	if err != nil {
		return fmt.Errorf("in condition #%d: %w", n, err)
	}
	c.st.setString(v)
	return nil
}
