// The constrained `!!js` expression interpreter for composition configs.
// The official loader evaluates JavaScript against the loader context; a Go
// host cannot run JS, so this file implements a recursive-descent evaluator
// for exactly the expression shapes the shipped bundles use — member/call
// chains over `process` / `ctx` / `dshHomePath`, nullish coalescing, strict
// equality, and ternaries — and refuses every other construct loudly. An
// unevaluable expression must never be silently treated as a literal (that
// would write wrong config into a running entry).
package boot

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"dshgo/cordis"
	"dshgo/cordis/loader"
	"dshgo/homepaths"
)

// evalContext carries everything an expression may reference: the cordis
// context for `ctx.<service>` reads, plus the process-level facts.
type evalContext struct {
	ctx *cordis.Context
}

// evaluateConfig deep-evaluates every loader.RawExpression inside a config
// value (maps, slices, scalars) against the composition context.
func evaluateConfig(ctx *cordis.Context, value any) (any, error) {
	switch typed := value.(type) {
	case loader.RawExpression:
		return evaluateExpression(evalContext{ctx: ctx}, typed.String())
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			evaled, err := evaluateConfig(ctx, item)
			if err != nil {
				return nil, err
			}
			out[key] = evaled
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			evaled, err := evaluateConfig(ctx, item)
			if err != nil {
				return nil, err
			}
			out = append(out, evaled)
		}
		return out, nil
	default:
		return value, nil
	}
}

// evaluateExpression parses and runs one expression source.
func evaluateExpression(ec evalContext, source string) (any, error) {
	parser := &exprParser{src: source}
	value, err := parser.parseTernary(ec)
	if err != nil {
		return nil, err
	}
	parser.skipSpace()
	if parser.pos != len(parser.src) {
		return nil, fmt.Errorf("loader: !!js expression %q has trailing input at offset %d", source, parser.pos)
	}
	return value, nil
}

// --- recursive-descent parser ------------------------------------------------

type exprParser struct {
	src string
	pos int
}

func (p *exprParser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

// parseTernary: nullish ('?' ternary ':' ternary)?
func (p *exprParser) parseTernary(ec evalContext) (any, error) {
	cond, err := p.parseNullish(ec)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if !p.consume("?") {
		return cond, nil
	}
	whenTrue, err := p.parseTernary(ec)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if !p.consume(":") {
		return nil, p.fail("expected ':' in ternary")
	}
	whenFalse, err := p.parseTernary(ec)
	if err != nil {
		return nil, err
	}
	if isTruthy(cond) {
		return whenTrue, nil
	}
	return whenFalse, nil
}

// parseNullish: equality ('??' nullish)?
func (p *exprParser) parseNullish(ec evalContext) (any, error) {
	left, err := p.parseEquality(ec)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if !p.consume("??") {
		return left, nil
	}
	right, err := p.parseNullish(ec)
	if err != nil {
		return nil, err
	}
	if left == nil {
		return right, nil
	}
	return left, nil
}

// parseEquality: primary (('===' | '!==') primary)?
func (p *exprParser) parseEquality(ec evalContext) (any, error) {
	left, err := p.parsePrimary(ec)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.consume("===") {
		right, err := p.parsePrimary(ec)
		if err != nil {
			return nil, err
		}
		return strictEqual(left, right), nil
	}
	if p.consume("!==") {
		right, err := p.parsePrimary(ec)
		if err != nil {
			return nil, err
		}
		return !strictEqual(left, right), nil
	}
	return left, nil
}

// parsePrimary: literals, `undefined`, parenthesized sub-expressions, or a
// member/call chain.
func (p *exprParser) parsePrimary(ec evalContext) (any, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, p.fail("unexpected end of expression")
	}
	if p.consume("(") {
		value, err := p.parseTernary(ec)
		if err != nil {
			return nil, err
		}
		if !p.consume(")") {
			return nil, p.fail("expected ')' after parenthesized expression")
		}
		return value, nil
	}
	if p.consume("'") || p.consume("\"") {
		quote := p.src[p.pos-1]
		end := strings.IndexByte(p.src[p.pos:], quote)
		if end < 0 {
			return nil, p.fail("unterminated string literal")
		}
		literal := p.src[p.pos : p.pos+end]
		p.pos += end + 1
		return literal, nil
	}
	if p.consume("undefined") {
		return nil, nil
	}
	if number, ok := p.tryNumber(); ok {
		return number, nil
	}
	// Member/call chain: ident('.'ident|'(' args ')')*
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || c >= 0x80 ||
			(c >= '0' && c <= '9' && p.pos > start) ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		return nil, p.fail(fmt.Sprintf("unexpected %q", string(p.src[start])))
	}
	chain := []string{p.src[start:p.pos]}
	for {
		p.skipSpace()
		if p.consume(".") {
			p.skipSpace()
			name := p.consumeIdent()
			if name == "" {
				return nil, p.fail("expected member name after '.'")
			}
			chain = append(chain, name)
			continue
		}
		if p.consume("(") {
			args, err := p.parseArgs(ec)
			if err != nil {
				return nil, err
			}
			value, err := evalCall(ec, chain, args)
			if err != nil {
				return nil, err
			}
			return value, nil
		}
		break
	}
	return evalChain(ec, chain)
}

// parseArgs parses a comma-separated argument list up to ')'.
func (p *exprParser) parseArgs(ec evalContext) ([]any, error) {
	args := []any{}
	p.skipSpace()
	if p.consume(")") {
		return args, nil
	}
	for {
		arg, err := p.parseTernary(ec)
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		p.skipSpace()
		if p.consume(",") {
			p.skipSpace()
			continue
		}
		if p.consume(")") {
			return args, nil
		}
		return nil, p.fail("expected ',' or ')' in argument list")
	}
}

func (p *exprParser) consume(token string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.src[p.pos:], token) {
		p.pos += len(token)
		return true
	}
	return false
}

func (p *exprParser) consumeIdent() string {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || (c >= '0' && c <= '9' && p.pos > start) ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			p.pos++
			continue
		}
		break
	}
	return p.src[start:p.pos]
}

func (p *exprParser) tryNumber() (any, bool) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) && (p.src[p.pos] >= '0' && p.src[p.pos] <= '9' || p.src[p.pos] == '.') {
		p.pos++
	}
	if p.pos == start {
		return nil, false
	}
	number, err := strconv.ParseFloat(p.src[start:p.pos], 64)
	if err != nil {
		p.pos = start
		return nil, false
	}
	return number, true
}

func (p *exprParser) fail(message string) error {
	return fmt.Errorf("loader: !!js expression %q: %s at offset %d", p.src, message, p.pos)
}

// --- evaluation --------------------------------------------------------------

// evalChain resolves a member chain against the expression context:
// `process.env.X` / `process.platform` / `process.cwd` (call follows) /
// `ctx.<service>.<field...>`.
func evalChain(ec evalContext, chain []string) (any, error) {
	switch chain[0] {
	case "process":
		return evalProcessChain(chain[1:])
	case "ctx":
		return evalServiceChain(ec.ctx, chain[1:])
	case "undefined":
		return nil, nil
	default:
		return nil, fmt.Errorf("loader: !!js expression references unsupported root %q (supported: process, ctx, dshHomePath)", chain[0])
	}
}

func evalProcessChain(chain []string) (any, error) {
	if len(chain) == 0 {
		return nil, fmt.Errorf("loader: !!js bare `process` is not supported")
	}
	switch chain[0] {
	case "env":
		if len(chain) != 2 {
			return nil, fmt.Errorf("loader: !!js unsupported process.env form %v", chain)
		}
		value, ok := os.LookupEnv(chain[1])
		if !ok || value == "" {
			return nil, nil
		}
		return value, nil
	case "platform":
		return nodePlatform(), nil
	case "cwd":
		if len(chain) != 1 {
			return nil, fmt.Errorf("loader: !!js unsupported process.cwd form %v", chain)
		}
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("loader: !!js process.cwd: %w", err)
		}
		return wd, nil
	default:
		return nil, fmt.Errorf("loader: !!js unsupported process member %q", chain[0])
	}
}

// nodePlatform maps runtime.GOOS to the Node process.platform literal.
func nodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	default:
		return runtime.GOOS
	}
}

// evalServiceChain reads `ctx.<service>(.field)*` from the composition
// context: the service value must be a map (flag-provided services are
// provided as plain maps) whose keys the chain then walks.
func evalServiceChain(ctx *cordis.Context, chain []string) (any, error) {
	if len(chain) == 0 {
		return nil, fmt.Errorf("loader: !!js bare `ctx` is not supported")
	}
	service := ctx.Get(chain[0])
	if service == nil {
		return nil, nil
	}
	value := service
	for _, field := range chain[1:] {
		record, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("loader: !!js cannot read field %q of non-object service %q", field, chain[0])
		}
		value = record[field]
	}
	return value, nil
}

// evalCall resolves a call chain: the only supported call is
// `dshHomePath('<relative>')` — the harness-home join the shipped bundles use.
func evalCall(ec evalContext, chain []string, args []any) (any, error) {
	if len(chain) == 1 && chain[0] == "dshHomePath" {
		if len(args) != 1 {
			return nil, fmt.Errorf("loader: !!js dshHomePath expects exactly one argument")
		}
		relative, ok := args[0].(string)
		if !ok {
			return nil, fmt.Errorf("loader: !!js dshHomePath argument must be a string literal")
		}
		return filepath.Join(homepaths.ResolveDshHome("", nil), filepath.FromSlash(relative)), nil
	}
	if len(chain) == 2 && chain[0] == "process" && chain[1] == "cwd" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("loader: !!js process.cwd: %w", err)
		}
		return wd, nil
	}
	return nil, fmt.Errorf("loader: !!js call %v is not supported by the Go evaluator", chain)
}

// strictEqual is the `===` analogue over the value domain the evaluator
// produces (nil, string, float64, bool).
func strictEqual(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left == right
}

// isTruthy applies JS truthiness over the evaluator's value domain.
func isTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	default:
		return true
	}
}
