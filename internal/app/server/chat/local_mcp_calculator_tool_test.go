package chat

import (
	"strings"
	"testing"
)

// TestCalculateExactResults 覆盖精度相关的核心场景。
// 这些正是模型心算容易出错、也是上一版 float64 方案算不对的地方。
func TestCalculateExactResults(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want string
	}{
		{"加法", "1+2", "3"},
		{"减法得负", "3-5", "-2"},
		{"乘法", "12*12", "144"},
		{"除法整除", "100/4", "25"},
		{"混合优先级", "2+3*4", "14"},
		{"括号改变优先级", "(2+3)*4", "20"},
		{"嵌套括号", "((1+2)*(3+4))", "21"},

		// float64 会给 0.30000000000000004，Rat 必须给出 0.3
		{"小数精确相加", "0.1+0.2", "0.3"},
		// float64 会给 0.9999999999999999 或 1.0000000000000002
		{"分数往返不失真", "1/3*3", "1"},
		{"多次小数运算", "0.1*3", "0.3"},

		// 理论值 1267650600228229401496703205376，float64 只能给 1.2676506002282294e30
		{"2的100次方", "2^100", "1267650600228229401496703205376"},
		{"18位大数相乘", "123456789012345678 * 987654321098765432", "121932631137021794322511812221002896"},
		{"七位数相乘", "1234567*7654321", "9449772114007"},

		{"右结合幂", "2^3^2", "512"},
		{"负指数", "2^-3", "0.125"},
		{"幂优先于一元负号", "-2^2", "-4"},
		{"括号内的负底数", "(-2)^2", "4"},
		{"零次幂", "5^0", "1"},

		{"取模", "7%3", "1"},
		{"科学计数法", "1.5e3", "1500"},
		{"带空格", " 1 +  2 * 3 ", "7"},
		{"一元正号", "+5+3", "8"},
		{"abs", "abs(-7)", "7"},
		{"sqrt完全平方", "sqrt(16)", "4"},
		{"sqrt参与运算", "sqrt(9)+1", "4"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			value, err := evaluateExpression(c.expr)
			if err != nil {
				t.Fatalf("evaluateExpression(%q) 返回错误: %v", c.expr, err)
			}
			got, _ := formatRat(value)
			if got != c.want {
				t.Fatalf("evaluateExpression(%q) = %s, 期望 %s", c.expr, got, c.want)
			}
		})
	}
}

// TestCalculateFractionForm 验证除不尽时同时给出小数与精确分数。
func TestCalculateFractionForm(t *testing.T) {
	cases := []struct {
		expr      string
		wantValue string
		wantExact string
	}{
		{"1/3", "0.33333333333333333333", "1/3"},
		{"2/3", "0.66666666666666666667", "2/3"},
		{"0.1+0.2", "0.3", "3/10"},
		{"1/8", "0.125", "1/8"},
	}

	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			value, err := evaluateExpression(c.expr)
			if err != nil {
				t.Fatalf("evaluateExpression(%q) 返回错误: %v", c.expr, err)
			}
			gotValue, gotExact := formatRat(value)
			if gotValue != c.wantValue {
				t.Fatalf("%q 小数为 %s, 期望 %s", c.expr, gotValue, c.wantValue)
			}
			if gotExact != c.wantExact {
				t.Fatalf("%q 分数为 %s, 期望 %s", c.expr, gotExact, c.wantExact)
			}
		})
	}
}

// TestCalculateNormalization 验证全角符号与千分位逗号被正确归一化。
// 模型和 ASR 都可能给出这些写法，不处理会白白浪费一次工具调用。
func TestCalculateNormalization(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"（1+2）×3", "9"},
		{"10÷4", "2.5"},
		{"1−2", "-1"},
		{"2**10", "1024"},
		{"1,000+1", "1001"},
		{"（2＋3）＊4", "20"},
	}

	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			value, err := evaluateExpression(c.expr)
			if err != nil {
				t.Fatalf("evaluateExpression(%q) 返回错误: %v", c.expr, err)
			}
			got, _ := formatRat(value)
			if got != c.want {
				t.Fatalf("evaluateExpression(%q) = %s, 期望 %s", c.expr, got, c.want)
			}
		})
	}
}

// TestCalculateRejectsInvalidInput 保证非法输入返回错误而不是 panic 或错值。
func TestCalculateRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{"空表达式", ""},
		{"只有空格", "   "},
		{"缺右操作数", "1+"},
		{"除零", "1/0"},
		{"除零(括号)", "5/(3-3)"},
		{"取模零", "5%0"},
		{"括号未闭合", "(1+2"},
		{"多余右括号", "1+2)"},
		{"两个小数点", "1..2"},
		{"纯字母", "abc"},
		{"未知函数", "log(10)"},
		{"负数开方", "sqrt(-4)"},
		{"非整数指数", "2^0.5"},
		{"浮点取模", "5.5%2"},
		{"尾随垃圾", "1+2xyz"},
		{"两个运算符", "1+*2"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := evaluateExpression(c.expr); err == nil {
				t.Fatalf("evaluateExpression(%q) 应当报错，却返回了结果", c.expr)
			}
		})
	}
}

// TestCalculateSizeGuards 验证规模防护。
// 服务器只有 406MB 内存，一次 9^99999999 就足以把整站打到 OOM。
func TestCalculateSizeGuards(t *testing.T) {
	t.Run("指数超过上限", func(t *testing.T) {
		if _, err := evaluateExpression("2^1001"); err == nil {
			t.Fatal("2^1001 应当被指数上限拦下")
		}
	})

	t.Run("结果过大", func(t *testing.T) {
		// 1000^1000 约 3001 位十进制，超过 1200 位上限
		if _, err := evaluateExpression("1000^1000"); err == nil {
			t.Fatal("1000^1000 应当被结果位数上限拦下")
		}
	})

	t.Run("表达式过长", func(t *testing.T) {
		long := ""
		for i := 0; i < maxExpressionLen; i++ {
			long += "1+"
		}
		long += "1"
		if _, err := evaluateExpression(long); err == nil {
			t.Fatal("超长表达式应当被拦下")
		}
	})

	t.Run("上限内的最大幂仍可计算", func(t *testing.T) {
		// 9^1000 是 955 位十进制，低于 1200 位上限，必须算得出来
		value, err := evaluateExpression("9^1000")
		if err != nil {
			t.Fatalf("9^1000 应当可以计算: %v", err)
		}
		got, _ := formatRat(value)
		if len(got) != 955 {
			t.Fatalf("9^1000 应有 955 位十进制，实际 %d 位", len(got))
		}
	})
}

// TestCalculateHandler 验证 handler 层的参数处理与响应结构。
func TestCalculateHandler(t *testing.T) {
	t.Run("正常表达式", func(t *testing.T) {
		out, err := calculateHandler(nil, `{"expression":"(1234567+89)*3.5"}`)
		if err != nil {
			t.Fatalf("handler 返回错误: %v", err)
		}
		if !strings.Contains(out, "（精确值") && !strings.Contains(out, "= ") {
			t.Fatalf("响应里没有结果: %s", out)
		}
	})

	t.Run("空参数", func(t *testing.T) {
		out, err := calculateHandler(nil, `{}`)
		if err != nil {
			t.Fatalf("handler 返回错误: %v", err)
		}
		if !strings.Contains(out, `"success":false`) {
			t.Fatalf("空表达式应返回失败响应: %s", out)
		}
	})

	t.Run("参数 JSON 非法", func(t *testing.T) {
		out, err := calculateHandler(nil, `{"expression":`)
		if err != nil {
			t.Fatalf("handler 返回错误: %v", err)
		}
		if !strings.Contains(out, `"success":false`) {
			t.Fatalf("非法 JSON 应返回失败响应: %s", out)
		}
	})

	t.Run("计算失败", func(t *testing.T) {
		out, err := calculateHandler(nil, `{"expression":"1/0"}`)
		if err != nil {
			t.Fatalf("handler 返回错误: %v", err)
		}
		if !strings.Contains(out, `"success":false`) {
			t.Fatalf("除零应返回失败响应: %s", out)
		}
	})
}
