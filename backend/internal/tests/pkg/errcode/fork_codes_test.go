package errcode_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"video-canvas/internal/pkg/errcode"
)

func TestForkErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    *errcode.Error
		code   int
		msg    string
		status int
	}{
		{"幂等冲突", errcode.ErrIdempotencyConflict, 90001, "幂等键已用于其他请求", http.StatusConflict},
		{"清单过期", errcode.ErrDramaManifestStale, 91001, "剧本分镜版本已变更，请刷新清单", http.StatusConflict},
		{"报价过期", errcode.ErrDramaQuoteExpired, 91002, "生成报价已失效，请重新获取报价", http.StatusConflict},
		{"剪辑参数非法", errcode.ErrDramaEditInvalid, 91003, "分镜剪辑参数不合法", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Code != tc.code || tc.err.Msg != tc.msg || tc.err.HTTPStatus() != tc.status {
				t.Fatalf("错误契约不一致：got=(%d, %q, %d) want=(%d, %q, %d)",
					tc.err.Code, tc.err.Msg, tc.err.HTTPStatus(), tc.code, tc.msg, tc.status)
			}
		})
	}
}

// TestErrorCodeDefinitions_UniqueForkRange 检查全部生产 New 定义，防止重号或上游/Fork 号段互相占用。
func TestErrorCodeDefinitions_UniqueForkRange(t *testing.T) {
	files, err := filepath.Glob("../../../pkg/errcode/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("读取 errcode 源码失败：files=%v err=%v", files, err)
	}
	seen := map[int64]string{}
	forkCount := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		forkCount += checkErrorCodeFile(t, path, seen)
	}
	if forkCount == 0 {
		t.Fatal("fork_codes.go 必须定义 Fork 错误码")
	}
}

func checkErrorCodeFile(t *testing.T, path string, seen map[int64]string) int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	forkFile := filepath.Base(path) == "fork_codes.go"
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "New" || len(call.Args) == 0 {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.INT {
			t.Errorf("%s: New 错误码必须是整数字面量", fset.Position(call.Pos()))
			return true
		}
		code, parseErr := strconv.ParseInt(literal.Value, 0, 64)
		if parseErr != nil {
			t.Errorf("解析错误码失败：%v", parseErr)
			return true
		}
		location := fset.Position(call.Pos()).String()
		if prev, duplicate := seen[code]; duplicate {
			t.Errorf("错误码 %d 重复：%s 与 %s", code, prev, location)
		}
		seen[code] = location
		if reserved := code >= 90000 && code <= 99999; forkFile != reserved {
			t.Errorf("%s: 错误码 %d 的 90000–99999 保留区归属错误", location, code)
		}
		if forkFile {
			count++
		}
		return true
	})
	return count
}
