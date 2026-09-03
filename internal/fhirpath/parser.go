package fhirpath

import (
	"fmt"
	"strings"
)

// Parse compiles a FHIRPath expression into an AST.
//
// Parsing is separated from evaluation so expressions can be compiled once and
// reused: the conformance index holds thousands of search parameter and
// invariant expressions, and re-parsing them per resource would dominate
// indexing cost.
func Parse(src string) (Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	e, err := p.parseExpr(precLowest)
	if err != nil {
		return nil, err
	}
	if tok := p.peek(); tok.Kind != EOF {
		return nil, p.errorf(tok.Pos, "unexpected %s after a complete expression", tok)
	}
	return e, nil
}

// MustParse is Parse for expressions known good at development time, such as
// literals in tests.
func MustParse(src string) Expr {
	e, err := Parse(src)
	if err != nil {
		panic(err)
	}
	return e
}

// Precedence levels, lowest binding first. The order is the specification's
// grammar read from its lowest-precedence alternative upward; getting it wrong
// silently changes the meaning of real expressions, so the ladder is spelled
// out explicitly rather than inferred from a table.
const (
	precLowest = iota
	precImplies
	precOr  // or, xor
	precAnd // and
	precMembership
	precEquality   // = ~ != !~
	precInequality // < <= > >=
	precUnion      // |
	precType       // is, as
	precAdditive   // + - &
	precMultiplicative
)

type parser struct {
	src  string
	toks []Token
	pos  int
}

func (p *parser) peek() Token { return p.toks[p.pos] }

func (p *parser) next() Token {
	t := p.toks[p.pos]
	if t.Kind != EOF {
		p.pos++
	}
	return t
}

func (p *parser) errorf(pos int, format string, args ...any) error {
	return &Error{Msg: sprintf(format, args...), Pos: pos, Src: p.src}
}

// parseExpr is precedence climbing: parse a unary/postfix operand, then absorb
// binary operators while they bind at least as tightly as the caller allows.
func (p *parser) parseExpr(minPrec int) (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		op, prec, ok := p.binaryOp()
		if !ok || prec < minPrec {
			return left, nil
		}
		tok := p.next()

		// "is" and "as" take a type name on the right, not an expression.
		if op == "is" || op == "as" {
			name, err := p.typeSpecifier()
			if err != nil {
				return nil, err
			}
			left = &TypeOp{Op: op, Operand: left, Type: name}
			continue
		}

		// Every FHIRPath binary operator is left-associative, so the right side
		// is parsed at the next tighter level.
		right, err := p.parseExpr(prec + 1)
		if err != nil {
			return nil, err
		}
		if right == nil {
			return nil, p.errorf(tok.Pos, "missing right operand for %q", op)
		}
		left = &Binary{Op: op, Left: left, Right: right}
	}
}

// binaryOp inspects the current token as a binary operator without consuming
// it. Word operators are matched on identifier text, since the lexer does not
// treat them as keywords -- see the note on Kind.
func (p *parser) binaryOp() (op string, prec int, ok bool) {
	t := p.peek()
	switch t.Kind {
	case Pipe:
		return "|", precUnion, true
	case Eq:
		return "=", precEquality, true
	case Equiv:
		return "~", precEquality, true
	case Neq:
		return "!=", precEquality, true
	case NotEquiv:
		return "!~", precEquality, true
	case Lt:
		return "<", precInequality, true
	case Lte:
		return "<=", precInequality, true
	case Gt:
		return ">", precInequality, true
	case Gte:
		return ">=", precInequality, true
	case Plus:
		return "+", precAdditive, true
	case Minus:
		return "-", precAdditive, true
	case Amp:
		return "&", precAdditive, true
	case Star:
		return "*", precMultiplicative, true
	case Slash:
		return "/", precMultiplicative, true
	case Ident:
		switch t.Text {
		case "div", "mod":
			return t.Text, precMultiplicative, true
		case "is", "as":
			return t.Text, precType, true
		case "in", "contains":
			return t.Text, precMembership, true
		case "and":
			return "and", precAnd, true
		case "or", "xor":
			return t.Text, precOr, true
		case "implies":
			return "implies", precImplies, true
		}
	}
	return "", 0, false
}

// parseUnary handles polarity, then hands off to the postfix chain.
func (p *parser) parseUnary() (Expr, error) {
	t := p.peek()
	if t.Kind == Plus || t.Kind == Minus {
		p.next()
		op := "+"
		if t.Kind == Minus {
			op = "-"
		}
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &Unary{Op: op, Operand: operand}, nil
	}
	return p.parsePostfix()
}

// parsePostfix parses a term and then any chain of ".name", ".fn(...)", and
// "[index]" applied to it. These bind tighter than every binary operator.
func (p *parser) parsePostfix() (Expr, error) {
	e, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek().Kind {
		case Dot:
			p.next()
			e, err = p.parseInvocation(e)
			if err != nil {
				return nil, err
			}
		case LBracket:
			open := p.next()
			idx, err := p.parseExpr(precLowest)
			if err != nil {
				return nil, err
			}
			if p.peek().Kind != RBracket {
				return nil, p.errorf(open.Pos, "unclosed '[' (expected ']', found %s)", p.peek())
			}
			p.next()
			e = &Indexer{Subject: e, Index: idx}
		default:
			return e, nil
		}
	}
}

// parseInvocation parses the part after a dot: a name, a function call, or a
// special variable.
func (p *parser) parseInvocation(subject Expr) (Expr, error) {
	t := p.next()
	switch t.Kind {
	case Ident, DelimitedIdent:
		inv := &Invocation{Subject: subject, Name: t.Text, Delimited: t.Kind == DelimitedIdent}
		if p.peek().Kind == LParen {
			args, err := p.parseArgs()
			if err != nil {
				return nil, err
			}
			inv.Args = args
		}
		return inv, nil
	case Dollar:
		// "$this" is legal after a dot in expressions like "children().$this".
		return &Variable{Name: t.Text}, nil
	default:
		return nil, p.errorf(t.Pos, "expected a name after '.', found %s", t)
	}
}

// parseArgs parses a parenthesized, comma-separated argument list. It returns a
// non-nil empty slice for "()", which is what distinguishes a zero-argument
// call from a member access.
func (p *parser) parseArgs() ([]Expr, error) {
	open := p.next() // '('
	args := []Expr{}
	if p.peek().Kind == RParen {
		p.next()
		return args, nil
	}
	for {
		arg, err := p.parseExpr(precLowest)
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		switch p.peek().Kind {
		case Comma:
			p.next()
		case RParen:
			p.next()
			return args, nil
		default:
			return nil, p.errorf(open.Pos, "unclosed '(' (expected ',' or ')', found %s)", p.peek())
		}
	}
}

// parseTerm parses the atoms: literals, parenthesized expressions, variables,
// external constants, and leading invocations.
func (p *parser) parseTerm() (Expr, error) {
	t := p.peek()
	switch t.Kind {
	case LParen:
		open := p.next()
		e, err := p.parseExpr(precLowest)
		if err != nil {
			return nil, err
		}
		if p.peek().Kind != RParen {
			return nil, p.errorf(open.Pos, "unclosed '(' (expected ')', found %s)", p.peek())
		}
		p.next()
		return e, nil

	case Ident:
		// "true" and "false" are literals; every other bare word begins an
		// invocation against the context.
		if t.Text == "true" || t.Text == "false" {
			p.next()
			return &Literal{Kind: LitBoolean, Text: t.Text}, nil
		}
		return p.parseInvocation(nil)

	case DelimitedIdent:
		return p.parseInvocation(nil)

	case Dollar:
		p.next()
		return &Variable{Name: t.Text}, nil

	case Percent:
		p.next()
		name := p.next()
		switch name.Kind {
		case Ident:
			return &ExternalConstant{Name: name.Text}, nil
		case DelimitedIdent, String:
			return &ExternalConstant{Name: name.Text, Delimited: true}, nil
		default:
			return nil, p.errorf(name.Pos, "expected a name after '%%', found %s", name)
		}

	case String:
		p.next()
		return &Literal{Kind: LitString, Text: t.Text}, nil

	case DateTime:
		p.next()
		return &Literal{Kind: LitDateTime, Text: t.Text}, nil

	case LongNumber:
		p.next()
		return &Literal{Kind: LitLong, Text: t.Text}, nil

	case Number:
		p.next()
		return p.numberOrQuantity(t), nil

	case EmptyCollection:
		p.next()
		return &Literal{Kind: LitEmpty}, nil

	default:
		return nil, p.errorf(t.Pos, "expected an expression, found %s", t)
	}
}

// numberOrQuantity turns a bare number into a quantity when a unit follows:
// "4 days" or "4 'mg'". The unit must be a calendar-duration word or a quoted
// UCUM code -- an arbitrary following identifier is not a unit, since
// "1 and true" must stay a comparison rather than becoming the quantity "1 and".
func (p *parser) numberOrQuantity(num Token) Expr {
	switch t := p.peek(); t.Kind {
	case String:
		p.next()
		return &Literal{Kind: LitQuantity, Text: num.Text, Unit: t.Text, UnitQuoted: true}
	case Ident:
		if isCalendarUnit(t.Text) {
			p.next()
			return &Literal{Kind: LitQuantity, Text: num.Text, Unit: t.Text}
		}
	}
	return &Literal{Kind: LitNumber, Text: num.Text}
}

// calendarUnits are the time-valued words a quantity literal may use, singular
// and plural. They are distinct from UCUM codes: the specification defines
// "1 year" as a calendar duration, which is not always 365 days.
var calendarUnits = map[string]bool{
	"year": true, "years": true, "month": true, "months": true,
	"week": true, "weeks": true, "day": true, "days": true,
	"hour": true, "hours": true, "minute": true, "minutes": true,
	"second": true, "seconds": true, "millisecond": true, "milliseconds": true,
}

func isCalendarUnit(s string) bool { return calendarUnits[s] }

// typeSpecifier parses the right side of "is" or "as": a possibly namespace-
// qualified type name such as "Quantity" or "FHIR.Patient".
func (p *parser) typeSpecifier() (string, error) {
	t := p.next()
	if t.Kind != Ident && t.Kind != DelimitedIdent {
		return "", p.errorf(t.Pos, "expected a type name, found %s", t)
	}
	var b strings.Builder
	b.WriteString(t.Text)
	for p.peek().Kind == Dot {
		// Only continue the qualified name when a bare name follows; otherwise
		// the dot belongs to a following invocation, as in "(x as Quantity).value".
		if p.pos+1 < len(p.toks) {
			nt := p.toks[p.pos+1]
			if nt.Kind != Ident && nt.Kind != DelimitedIdent {
				break
			}
			// A function call after the dot is an invocation on the result of
			// the type operation, not part of the type name.
			if p.pos+2 < len(p.toks) && p.toks[p.pos+2].Kind == LParen {
				break
			}
		}
		p.next()
		part := p.next()
		b.WriteByte('.')
		b.WriteString(part.Text)
	}
	return b.String(), nil
}

// sprintf is fmt.Sprintf, wrapped so parser.errorf can stay dependency-light
// and uniform with the lexer's error construction.
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
