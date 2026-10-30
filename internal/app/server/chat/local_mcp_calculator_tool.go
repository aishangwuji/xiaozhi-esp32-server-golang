package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	log "xiaozhi-esp32-server-golang/logger"
)

// 计算器工具的规模上限。
//
// 这里用 math/big.Rat 而不是 float64：浮点只有 15~17 位十进制有效数字，
// 恰好就是"数量级一大就有偏差"的成因（2^100 的尾数会整个丢掉）。
// Rat 以分子/分母表示，整数与大整数运算永不丢精度，除法保留精确分数，
// 只在最终呈现时才转成十进制小数。
const (
	// 表达式长度上限。语音场景下的算式远短于这个值，限制它只是为了避免
	// 超长输入拖慢解析器。
	maxExpressionLen = 512
	// 幂运算指数绝对值上限。9^1000 已有约 954 位十进制，远超语音场景所需，
	// 同时把结果规模钉在可控范围内——服务器只有 406MB 内存，一次 9^99999999
	// 就足以把整站打到 OOM（见 AGENTS.md 3.3 的历史事故）。
	maxPowerExponent = 1000
	// 结果十进制位数上限。
	maxResultDigits = 1200
	// 输出小数的有效位数。
	decimalPrecision = 20
)

// CalculateParams calculate 工具入参
type CalculateParams struct {
	Expression string `json:"expression" description:"要计算的数学表达式，使用 + - * / % ^ 与括号，例如 (1234567+89)*3.5 或 2^100。支持 sqrt() 和 abs()。只放数字与运算符，不要带单位或中文说明" required:"true"`
}

// calculateHandler 精确计算数学表达式。
//
// 结果通过 NewContentResponse 返回：message 是给模型转述的一句话，
// data 里带结构化字段（expression / result / exact）。
func calculateHandler(ctx context.Context, argumentsInJSON string) (string, error) {
	var params CalculateParams
	if argumentsInJSON != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &params); err != nil {
			response := NewErrorResponse("calculate", "参数解析失败", "PARSE_ERROR", "请检查 expression 参数格式")
			return response.ToJSON()
		}
	}

	expr := strings.TrimSpace(params.Expression)
	if expr == "" {
		response := NewErrorResponse("calculate", "expression 不能为空", "EMPTY_EXPRESSION", "请提供要计算的数学表达式")
		return response.ToJSON()
	}

	value, err := evaluateExpression(expr)
	if err != nil {
		log.Infof("calculate 失败: expression=%q err=%v", expr, err)
		response := NewErrorResponse("calculate", err.Error(), "CALC_ERROR", "请检查表达式是否合法，或改成分步计算")
		return response.ToJSON()
	}

	result, exact := formatRat(value)
	if len(result) > maxResultDigits {
		response := NewErrorResponse("calculate",
			fmt.Sprintf("结果超过 %d 位，请缩小规模", maxResultDigits),
			"RESULT_TOO_LARGE", "请改用数量级更小的表达式，或改成分步计算")
		return response.ToJSON()
	}

	data := map[string]any{
		"expression": expr,
		"result":     result,
	}
	message := fmt.Sprintf("%s = %s", expr, result)
	if exact != "" {
		data["exact"] = exact
		message = fmt.Sprintf("%s = %s（精确值 %s）", expr, result, exact)
	}

	log.Infof("calculate 成功: expression=%q result=%s", expr, result)
	response := NewContentResponse("calculate", data, message)
	return response.ToJSON()
}

// evaluateExpression 归一化并求解表达式，返回精确有理数。
func evaluateExpression(input string) (*big.Rat, error) {
	normalized, err := normalizeExpression(input)
	if err != nil {
		return nil, err
	}

	p := &exprParser{s: normalized}
	value, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	p.skipSpaces()
	if p.pos < len(p.s) {
		return nil, fmt.Errorf("第 %d 个字符处有无法识别的内容 %q", p.pos+1, p.s[p.pos:])
	}
	return value, nil
}

// normalizeExpression 把常见的全角符号与语音转写残留统一成解析器认识的写法。
//
// 模型给出的表达式偶尔会带 ×÷−，或 ASR 转写时插入千分位逗号，这些不处理
// 就会直接报"无法识别"，白白浪费一轮工具调用。
func normalizeExpression(input string) (string, error) {
	expr := strings.TrimSpace(input)
	if expr == "" {
		return "", errors.New("表达式为空")
	}
	if len(expr) > maxExpressionLen {
		return "", fmt.Errorf("表达式过长，最大支持 %d 个字符", maxExpressionLen)
	}

	replacer := strings.NewReplacer(
		"（", "(",
		"）", ")",
		"×", "*",
		"✕", "*",
		"＊", "*",
		"÷", "/",
		"／", "/",
		"＋", "+",
		"－", "-",
		"−", "-",
		"–", "-",
		"—", "-",
		"％", "%",
		"**", "^",
		"，", "",
		",", "",
		"\u00a0", " ",
		"\u3000", " ",
	)
	return replacer.Replace(expr), nil
}

// exprParser 是递归下降解析器，直接在字符串上扫描。
//
// 文法（优先级由低到高）：
//
//	expr    := term (('+' | '-') term)*
//	term    := unary (('*' | '/' | '%') unary)*
//	unary   := ('-' | '+') unary | power
//	power   := primary ('^' unary)?        // 右结合，指数允许带符号
//	primary := number | '(' expr ')' | func '(' expr ')'
//
// `^` 绑定比一元负号更紧，所以 -2^2 解析为 -(2^2) = -4，与数学惯例一致。
type exprParser struct {
	s   string
	pos int
}

func (p *exprParser) skipSpaces() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t' || p.s[p.pos] == '\n' || p.s[p.pos] == '\r') {
		p.pos++
	}
}

func (p *exprParser) peek() byte {
	if p.pos >= len(p.s) {
		return 0
	}
	return p.s[p.pos]
}

func (p *exprParser) parseExpr() (*big.Rat, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpaces()
		op := p.peek()
		if op != '+' && op != '-' {
			return left, nil
		}
		p.pos++
		right, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		if op == '+' {
			left = new(big.Rat).Add(left, right)
		} else {
			left = new(big.Rat).Sub(left, right)
		}
	}
}

func (p *exprParser) parseTerm() (*big.Rat, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpaces()
		op := p.peek()
		if op != '*' && op != '/' && op != '%' {
			return left, nil
		}
		p.pos++
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		switch op {
		case '*':
			left = new(big.Rat).Mul(left, right)
		case '/':
			// big.Rat.Quo 除以零会 panic，必须先挡掉。
			if right.Sign() == 0 {
				return nil, errors.New("除数不能为 0")
			}
			left = new(big.Rat).Quo(left, right)
		case '%':
			mod, err := ratMod(left, right)
			if err != nil {
				return nil, err
			}
			left = mod
		}
	}
}

func (p *exprParser) parseUnary() (*big.Rat, error) {
	p.skipSpaces()
	switch p.peek() {
	case '-':
		p.pos++
		value, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return new(big.Rat).Neg(value), nil
	case '+':
		p.pos++
		return p.parseUnary()
	}
	return p.parsePower()
}

func (p *exprParser) parsePower() (*big.Rat, error) {
	base, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	p.skipSpaces()
	if p.peek() != '^' {
		return base, nil
	}
	p.pos++
	// 右侧走 parseUnary：既保证右结合（2^3^2 = 2^9），也允许负指数（2^-1）。
	exp, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	return ratPow(base, exp)
}

func (p *exprParser) parsePrimary() (*big.Rat, error) {
	p.skipSpaces()
	if p.pos >= len(p.s) {
		return nil, errors.New("表达式不完整")
	}

	c := p.s[p.pos]
	switch {
	case c == '(':
		p.pos++
		value, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		p.skipSpaces()
		if p.peek() != ')' {
			return nil, errors.New("括号不匹配，缺少 )")
		}
		p.pos++
		return value, nil
	case isDigit(c) || c == '.':
		return p.parseNumber()
	case isAlpha(c):
		return p.parseFunction()
	}
	return nil, fmt.Errorf("第 %d 个字符 %q 无法识别", p.pos+1, string(c))
}

func (p *exprParser) parseNumber() (*big.Rat, error) {
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if isDigit(c) || c == '.' {
			p.pos++
			continue
		}
		// 科学计数法：e/E 后必须跟数字，可带正负号。否则这个 e 属于其它内容。
		if c == 'e' || c == 'E' {
			next := p.pos + 1
			if next < len(p.s) && (p.s[next] == '+' || p.s[next] == '-') {
				next++
			}
			if next < len(p.s) && isDigit(p.s[next]) {
				p.pos = next + 1
				continue
			}
		}
		break
	}

	text := p.s[start:p.pos]
	if text == "" {
		return nil, fmt.Errorf("第 %d 个字符处缺少数字", start+1)
	}
	value, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, fmt.Errorf("%q 不是合法数字", text)
	}
	return value, nil
}

func (p *exprParser) parseFunction() (*big.Rat, error) {
	start := p.pos
	for p.pos < len(p.s) && isAlpha(p.s[p.pos]) {
		p.pos++
	}
	name := strings.ToLower(p.s[start:p.pos])

	p.skipSpaces()
	if p.peek() != '(' {
		return nil, fmt.Errorf("函数 %s 后面缺少 (", name)
	}
	p.pos++

	arg, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	p.skipSpaces()
	if p.peek() != ')' {
		return nil, fmt.Errorf("函数 %s 的括号不匹配", name)
	}
	p.pos++

	switch name {
	case "sqrt":
		return ratSqrt(arg)
	case "abs":
		return new(big.Rat).Abs(arg), nil
	}
	return nil, fmt.Errorf("不支持的函数 %s（仅支持 sqrt 和 abs）", name)
}

// ratPow 计算 base^exp。只接受整数指数——非整数指数属于开方范畴，
// 而开方结果一般是无理数，与"精确计算"的承诺冲突，交给 sqrt() 明示。
func ratPow(base, exp *big.Rat) (*big.Rat, error) {
	if !exp.IsInt() {
		return nil, errors.New("指数必须是整数，开方请使用 sqrt()")
	}

	n := new(big.Int).Set(exp.Num())
	negative := n.Sign() < 0
	if negative {
		n.Neg(n)
	}
	if n.Cmp(big.NewInt(maxPowerExponent)) > 0 {
		return nil, fmt.Errorf("指数过大，最大支持 %d", maxPowerExponent)
	}
	if negative && base.Sign() == 0 {
		return nil, errors.New("0 不能作为负指数")
	}

	e := int(n.Int64())
	result := new(big.Rat).SetInt64(1)
	factor := new(big.Rat).Set(base)
	for e > 0 {
		if e&1 == 1 {
			result.Mul(result, factor)
		}
		e >>= 1
		if e > 0 {
			factor.Mul(factor, factor)
		}
		// 提前拦截规模爆炸，别等算完再判断，中间值本身就可能吃掉内存。
		if result.Num().BitLen() > maxResultDigits*4 || result.Denom().BitLen() > maxResultDigits*4 {
			return nil, fmt.Errorf("结果过大，请缩小规模")
		}
	}

	if negative {
		result.Inv(result)
	}
	return result, nil
}

// ratMod 取模。只对整数有意义，浮点取模的语义在各语言间不一致，不值得猜。
func ratMod(a, b *big.Rat) (*big.Rat, error) {
	if !a.IsInt() || !b.IsInt() {
		return nil, errors.New("% 取模只支持整数操作数")
	}
	if b.Sign() == 0 {
		return nil, errors.New("取模的除数不能为 0")
	}
	mod := new(big.Int).Mod(a.Num(), b.Num())
	return new(big.Rat).SetInt(mod), nil
}

// ratSqrt 开平方。结果按 200 位精度求近似值，
// 因为无理数不可能用有理数精确表示，这里不假装它是精确的。
func ratSqrt(a *big.Rat) (*big.Rat, error) {
	if a.Sign() < 0 {
		return nil, errors.New("负数不能开平方")
	}
	f := new(big.Float).SetPrec(200).SetRat(a)
	f.Sqrt(f)
	value, _ := f.Rat(nil)
	return value, nil
}

// formatRat 把精确有理数转成可读字符串。
//
// 返回 (十进制结果, 精确分数形式)。分母是 1（整数）或分母规模过大
// （例如 sqrt 得到的近似值）时不返回分数形式。
func formatRat(r *big.Rat) (string, string) {
	if r.IsInt() {
		return r.Num().String(), ""
	}

	f := new(big.Float).SetPrec(256).SetRat(r)
	s := f.Text('f', decimalPrecision)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	if s == "" || s == "-" {
		s = "0"
	}

	exact := ""
	if r.Denom().BitLen() <= 64 {
		exact = r.RatString()
	}
	return s, exact
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}
