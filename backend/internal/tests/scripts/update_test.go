package scripts_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// update.sh 是升级和回退的唯一入口，出错时直接影响线上数据，所以用一个假的 docker 命令把它整条流程跑一遍：
// 假 docker 记下每次调用，按测试设定的文件决定拉镜像、备份、健康检查的结果，不需要 Docker 环境。

const fakeDocker = `#!/usr/bin/env bash
echo "$*" >> "$FAKE_DIR/calls.log"
rc() { cat "$FAKE_DIR/$1" 2>/dev/null || echo "${2:-0}"; }
case "$1" in
  info) exit 0 ;;
  compose)
    shift
    case "$1" in
      version) exit 0 ;;
      pull) exit "$(rc pull_rc)" ;;
      ps)
        case "$*" in
          *--format*) cat "$FAKE_DIR/health" ;;
          *"--status running --services"*) printf 'web\n' ;;
          *) echo ps ;;
        esac ;;
      exec)
        case "$*" in
          *pg_dump*) printf 'PGDMP-fake-dump' ;;
          *wget*) cat "$FAKE_DIR/health_json" ;;
        esac ;;
      run)
        code="$(rc agent_rc)"
        [ "$code" = 0 ] && cat "$FAKE_DIR/agent_dump"
        exit "$code" ;;
      up) exit 0 ;;
      logs) cat "$FAKE_DIR/logs_${@: -1}" 2>/dev/null || true ;;
    esac ;;
esac
exit 0
`

const fakeCurl = `#!/usr/bin/env bash
echo "$*" >> "$FAKE_DIR/curl_calls.log"
rc() { cat "$FAKE_DIR/$1" 2>/dev/null || echo "${2:-0}"; }
out=""
url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -H|--max-time) shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
global_rc="$(rc curl_rc 0)"
[ "$global_rc" = "0" ] || exit "$global_rc"
case "$url" in
  */docker-compose.yml)
    c_rc="$(rc curl_compose_rc 0)"
    [ "$c_rc" = "0" ] || exit "$c_rc"
    if [ -f "$FAKE_DIR/remote_compose" ]; then
      cat "$FAKE_DIR/remote_compose" > "$out"
    else
      printf 'services: {}\n' > "$out"
    fi
    ;;
  */scripts/update.sh)
    u_rc="$(rc curl_update_rc 0)"
    [ "$u_rc" = "0" ] || exit "$u_rc"
    if [ -f "$FAKE_DIR/remote_update" ]; then
      cat "$FAKE_DIR/remote_update" > "$out"
    else
      printf '#!/usr/bin/env bash\n# stub update.sh\n' > "$out"
    fi
    ;;
  */releases/latest)
    l_rc="$(rc curl_latest_rc 0)"
    [ "$l_rc" = "0" ] || exit "$l_rc"
    printf '{"tag_name":"v1.1.0"}\n'
    ;;
  *)
    exit 22
    ;;
esac
`

const sqliteDump = "SQLite format 3\x00fake-agent-data"

type env struct {
	t    *testing.T
	root string // 部署目录
	fake string // 假 docker 的状态目录
	bin  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, root: t.TempDir(), fake: t.TempDir(), bin: t.TempDir()}
	write := func(path, content string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(e.bin, "docker"), fakeDocker, 0o755)
	write(filepath.Join(e.bin, "curl"), fakeCurl, 0o755)
	envFile := "IMAGE_TAG=1.0.0\nPOSTGRES_USER=postgres\nPOSTGRES_DB=video_canvas\nAGENT_SERVICE_TOKEN=" + strings.Repeat("a", 64) + "\n"
	write(filepath.Join(e.root, ".env"), envFile, 0o644)
	write(filepath.Join(e.root, "docker-compose.yml"), "services: {}\n", 0o644)
	e.set("health", "backend healthy\nagent healthy\nweb\n")
	e.set("health_json", `{"status":"ok","version":"v1.1.0"}`)
	e.set("agent_dump", sqliteDump)
	return e
}

func (e *env) set(name, content string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.fake, name), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// run 执行 update.sh，返回合并的输出、退出码和状态文件的内容。
func (e *env) run(args ...string) (string, int, map[string]string) {
	e.t.Helper()
	script, _ := filepath.Abs("../../../../scripts/update.sh")
	status := filepath.Join(e.root, "status.json")
	cmd := exec.Command("bash", append([]string{script, "--dir", e.root, "--status-file", status}, args...)...)
	cmd.Env = []string{"PATH=" + e.bin + ":" + os.Getenv("PATH"), "FAKE_DIR=" + e.fake, "HOME=" + e.root}
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	st := map[string]string{}
	if b, err := os.ReadFile(status); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return string(out), code, st
}

func (e *env) calls() string {
	b, _ := os.ReadFile(filepath.Join(e.fake, "calls.log"))
	return string(b)
}

func (e *env) backups(pattern string) []string {
	m, _ := filepath.Glob(filepath.Join(e.root, "backups", pattern))
	return m
}

func (e *env) envValue(key string) string {
	b, _ := os.ReadFile(filepath.Join(e.root, ".env"))
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v
		}
	}
	return ""
}

func TestUpdate_BacksUpAgentDataOnline(t *testing.T) {
	e := newEnv(t)
	out, code, st := e.run("--tag", "1.1.0")
	if code != 0 || st["state"] != "succeeded" {
		t.Fatalf("应成功：%d %v\n%s", code, st, out)
	}
	dump := e.backups("pre-1.1.0-*.dump")
	agent := e.backups("pre-1.1.0-*.agent.sqlite")
	if len(dump) != 1 || len(agent) != 1 {
		t.Fatalf("数据库和 Agent 数据各一份备份：%v %v\n%s", dump, agent, out)
	}
	if b, _ := os.ReadFile(agent[0]); string(b) != sqliteDump {
		t.Errorf("备份内容应原样落盘：%q", b)
	}
	if len(e.backups(".agent-backup.tmp")) != 0 {
		t.Error("不能留下临时文件")
	}
	calls := e.calls()
	// 用目标版本的镜像起一次性容器备份：服务在不在运行都行；必须在重建（up）之前
	runAt, upAt := strings.Index(calls, "compose run --rm --no-deps -T --entrypoint sh agent"), strings.Index(calls, "compose up")
	if runAt < 0 || upAt < 0 || runAt > upAt {
		t.Errorf("Agent 备份应在重建之前：\n%s", calls)
	}
	if !strings.Contains(calls, "node dist/backup-cli.js") {
		t.Errorf("应调用镜像里的备份命令：\n%s", calls)
	}
}

func TestUpdate_NoBackupFlagSkipsBothBackups(t *testing.T) {
	e := newEnv(t)
	if _, code, _ := e.run("--tag", "1.1.0", "--no-backup"); code != 0 || strings.Contains(e.calls(), "compose run") || len(e.backups("*")) != 0 {
		t.Errorf("--no-backup 什么都不备份：\n%s", e.calls())
	}
}

// 从没有独立 Agent 服务的旧版本升上来：.env 里还没有服务令牌，update.sh 要生成，否则 backend 启用 Agent 时拒绝启动。
func TestUpdate_GeneratesAgentTokenWhenMissing(t *testing.T) {
	for name, envLine := range map[string]string{"没有这一项": "", "太短": "AGENT_SERVICE_TOKEN=short\n"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			raw, _ := os.ReadFile(filepath.Join(e.root, ".env"))
			stripped := regexp.MustCompile(`(?m)^AGENT_SERVICE_TOKEN=.*\n`).ReplaceAllString(string(raw), "")
			if err := os.WriteFile(filepath.Join(e.root, ".env"), []byte(stripped+envLine+"APP_AGENT_RUNTIME=http\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, code, _ := e.run("--tag", "1.1.0"); code != 0 {
				t.Fatalf("应成功：%s", out)
			}
			tok := e.envValue("AGENT_SERVICE_TOKEN")
			if len(tok) < 32 {
				t.Errorf("应生成至少 32 个字符的令牌：%q", tok)
			}
			if got := strings.Count(mustRead(t, filepath.Join(e.root, ".env")), "AGENT_SERVICE_TOKEN="); got != 1 {
				t.Errorf(".env 里只该有一行令牌：%d", got)
			}
		})
	}
}

// 已有合格的令牌就不动：backend 和 agent 两边都靠它，换掉会让正在运行的服务和新配置对不上。
func TestUpdate_KeepsExistingAgentToken(t *testing.T) {
	e := newEnv(t)
	before := e.envValue("AGENT_SERVICE_TOKEN")
	if out, code, _ := e.run("--tag", "1.1.0"); code != 0 {
		t.Fatalf("%s", out)
	}
	if got := e.envValue("AGENT_SERVICE_TOKEN"); got != before {
		t.Errorf("已有的令牌不能被改：%q → %q", before, got)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdate_FirstEnableHasNoAgentDataYet(t *testing.T) {
	e := newEnv(t)
	e.set("agent_rc", "3") // 卷里还没有数据库
	out, code, st := e.run("--tag", "1.1.0")
	if code != 0 || st["state"] != "succeeded" {
		t.Fatalf("第一次启用不该被备份卡住：%d %v\n%s", code, st, out)
	}
	if len(e.backups("*.agent.sqlite")) != 0 || len(e.backups("pre-1.1.0-*.dump")) != 1 {
		t.Errorf("只有数据库的备份：%v", e.backups("*"))
	}
	if !strings.Contains(out, "还没有数据") {
		t.Errorf("要说明为什么没备份：\n%s", out)
	}
}

func TestUpdate_AgentBackupFailureAbortsBeforeTouchingServices(t *testing.T) {
	for name, set := range map[string]func(e *env){
		"备份命令失败":      func(e *env) { e.set("agent_rc", "1") },
		"备份是空文件":      func(e *env) { e.set("agent_dump", "") },
		"备份不是 SQLite": func(e *env) { e.set("agent_dump", "<html>oops</html>") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			set(e)
			out, code, st := e.run("--tag", "1.1.0")
			if code == 0 || st["state"] != "failed" {
				t.Fatalf("应中止：%d %v\n%s", code, st, out)
			}
			if strings.Contains(e.calls(), "compose up") {
				t.Errorf("备份失败不能重建服务：\n%s", e.calls())
			}
			if got := e.envValue("IMAGE_TAG"); got != "1.0.0" {
				t.Errorf("IMAGE_TAG 应退回更新前的版本：%q", got)
			}
			if len(e.backups("*.agent.sqlite")) != 0 || len(e.backups(".agent-backup.tmp")) != 0 {
				t.Errorf("不能留下坏备份：%v", e.backups("*"))
			}
			if !strings.Contains(st["message"], "备份 Agent 数据失败") {
				t.Errorf("要说清楚原因：%q", st["message"])
			}
		})
	}
}

func TestUpdate_KeepsOnlyTheNewestBackupsOfEachKind(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Join(e.root, "backups"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		for _, ext := range []string{"dump", "agent.sqlite"} {
			f := filepath.Join(e.root, "backups", fmt.Sprintf("pre-0.%d.0-20260101000000.%s", i, ext))
			if err := os.WriteFile(f, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if out, code, _ := e.run("--tag", "1.1.0", "--keep-backups", "3"); code != 0 {
		t.Fatalf("%s", out)
	}
	for _, pat := range []string{"*.dump", "*.agent.sqlite"} {
		got := e.backups(pat)
		if len(got) != 3 {
			t.Errorf("%s 应只留最近 3 份：%v", pat, got)
		}
		for _, g := range got {
			if strings.Contains(g, "pre-0.1.0") || strings.Contains(g, "pre-0.2.0") {
				t.Errorf("最旧的应被清掉：%s", g)
			}
		}
	}
}

// 新版本起不来、退回旧版本后 agent 因为「数据已被新版本升级」拒绝启动：不能假装回退成功，要指到 Agent 的备份。
func TestUpdate_RollbackExplainsAgentDataNewerThanOldImage(t *testing.T) {
	e := newEnv(t)
	e.set("health", "backend healthy\nagent starting\nweb\n") // 一直不就绪
	e.set("logs_agent", `{"msg":"数据库的版本 4 比当前代码（最新是 3）新：可能回退了镜像。请换回新版镜像，或从升级前的备份恢复 agent_data 卷"}`)
	out, code, st := e.run("--tag", "1.1.0", "--timeout", "1")
	if code == 0 || st["state"] != "failed_needs_attention" {
		t.Fatalf("应报需要人工处理：%d %v\n%s", code, st, out)
	}
	agent := e.backups("pre-1.1.0-*.agent.sqlite")
	if len(agent) != 1 {
		t.Fatalf("应保留一份 Agent 备份：%v", agent)
	}
	// Git Bash 将 Windows 盘符路径转换成 /e/...；比较完整路径时统一到脚本使用的格式。
	backupPath := filepath.ToSlash(agent[0])
	if volume := filepath.VolumeName(agent[0]); len(volume) == 2 && volume[1] == ':' {
		backupPath = "/" + strings.ToLower(volume[:1]) + strings.TrimPrefix(backupPath, volume)
	}
	if !strings.Contains(st["message"], backupPath) || !strings.Contains(st["message"], "agent 拒绝启动") {
		t.Errorf("要指到 Agent 备份：%q（备份 %v）", st["message"], agent)
	}
	if got := e.envValue("IMAGE_TAG"); got != "1.0.0" {
		t.Errorf("回退后 IMAGE_TAG 应是旧版本：%q", got)
	}
}

// 回退时看到的新版本日志里要带上 agent 的（agent 起不来是最常见的原因之一）。
func TestUpdate_RollbackShowsAgentLogs(t *testing.T) {
	e := newEnv(t)
	e.set("health", "backend healthy\nagent starting\nweb\n")
	e.set("logs_agent", "AGENT-LOG-MARKER")
	out, _, _ := e.run("--tag", "1.1.0", "--timeout", "1")
	if !strings.Contains(out, "AGENT-LOG-MARKER") {
		t.Errorf("回退时应打印 agent 的日志：\n%s", out)
	}
}

func (e *env) curlCalls() string {
	b, _ := os.ReadFile(filepath.Join(e.fake, "curl_calls.log"))
	return string(b)
}

func TestUpdate_ForkRepoAndOwnerDefaultsAndOverrides(t *testing.T) {
	t.Run("defaults_to_hl9565_when_missing", func(t *testing.T) {
		e := newEnv(t)
		out, code, _ := e.run("--tag", "1.1.0")
		if code != 0 {
			t.Fatalf("应成功：%d\n%s", code, out)
		}
		if got := e.envValue("IMAGE_OWNER"); got != "hl9565" {
			t.Errorf("IMAGE_OWNER = %q, want hl9565", got)
		}
		if got := e.envValue("APP_UPDATE_REPO"); got != "hl9565/video-canvas" {
			t.Errorf("APP_UPDATE_REPO = %q, want hl9565/video-canvas", got)
		}
		cc := e.curlCalls()
		if !strings.Contains(cc, "https://raw.githubusercontent.com/hl9565/video-canvas/v1.1.0/docker-compose.yml") ||
			!strings.Contains(cc, "https://raw.githubusercontent.com/hl9565/video-canvas/v1.1.0/scripts/update.sh") {
			t.Errorf("未从 fork 仓库拉取目标 tag 的 compose/script：\n%s", cc)
		}
		if strings.Contains(strings.ToLower(out+cc), "zaylora") {
			t.Errorf("不应出现上游仓库回落：\n%s\n%s", out, cc)
		}
	})

	t.Run("preserves_existing_owner_and_repo", func(t *testing.T) {
		e := newEnv(t)
		raw := mustRead(t, filepath.Join(e.root, ".env"))
		if err := os.WriteFile(filepath.Join(e.root, ".env"), []byte(raw+"IMAGE_OWNER=custom-owner\nAPP_UPDATE_REPO=custom-owner/vc\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, code, _ := e.run("--tag", "1.1.0"); code != 0 {
			t.Fatalf("%s", out)
		}
		if got := e.envValue("IMAGE_OWNER"); got != "custom-owner" {
			t.Errorf("应保留已有 IMAGE_OWNER：%q", got)
		}
		if got := e.envValue("APP_UPDATE_REPO"); got != "custom-owner/vc" {
			t.Errorf("应保留已有 APP_UPDATE_REPO：%q", got)
		}
		if cc := e.curlCalls(); !strings.Contains(cc, "https://raw.githubusercontent.com/custom-owner/vc/v1.1.0/docker-compose.yml") {
			t.Errorf("应使用保留的仓库拉取：\n%s", cc)
		}
	})

	t.Run("repo_flag_overrides_update_repo", func(t *testing.T) {
		e := newEnv(t)
		if out, code, _ := e.run("--tag", "1.1.0", "--repo", "my-org/my-vc"); code != 0 {
			t.Fatalf("%s", out)
		}
		if got := e.envValue("APP_UPDATE_REPO"); got != "my-org/my-vc" {
			t.Errorf("--repo 应写入 APP_UPDATE_REPO：%q", got)
		}
		if cc := e.curlCalls(); !strings.Contains(cc, "https://raw.githubusercontent.com/my-org/my-vc/v1.1.0/scripts/update.sh") {
			t.Errorf("应按 --repo 拉取：\n%s", cc)
		}
	})

	t.Run("rejects_invalid_repo", func(t *testing.T) {
		e := newEnv(t)
		_, code, st := e.run("--tag", "1.1.0", "--repo", "../bad")
		if code == 0 || st["state"] != "failed" {
			t.Fatalf("非法仓库应直接拒绝：%d %v", code, st)
		}
		if strings.Contains(e.calls(), "compose pull") {
			t.Errorf("非法仓库不应继续拉镜像：\n%s", e.calls())
		}
	})
}

func TestUpdate_ComposeOrScriptDownloadFailureAbortsCleanly(t *testing.T) {
	for name, setup := range map[string]func(e *env){
		"compose下载失败": func(e *env) { e.set("curl_compose_rc", "22") },
		"脚本下载失败":      func(e *env) { e.set("curl_update_rc", "22") },
		"compose为空":   func(e *env) { e.set("remote_compose", "") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			setup(e)
			out, code, st := e.run("--tag", "1.1.0")
			if code == 0 || st["state"] != "failed" || st["phase"] != "compose" {
				t.Fatalf("应 fail-closed 中止：%d %v\n%s", code, st, out)
			}
			if got := e.envValue("IMAGE_TAG"); got != "1.0.0" {
				t.Errorf("IMAGE_TAG 应还原为 1.0.0：%q", got)
			}
			if got := mustRead(t, filepath.Join(e.root, "docker-compose.yml")); got != "services: {}\n" {
				t.Errorf("docker-compose.yml 不应被破坏：%q", got)
			}
			if strings.Contains(e.calls(), "compose pull") || strings.Contains(e.calls(), "compose up") {
				t.Errorf("下载配置失败绝不能继续拉镜像或重建：\n%s", e.calls())
			}
			if strings.Contains(strings.ToLower(out+e.curlCalls()), "zaylora") {
				t.Errorf("失败后绝不能回落其他仓库：\n%s\n%s", out, e.curlCalls())
			}
		})
	}
}

func TestUpdate_ComposeUpdateAndRollbackRestoresFiles(t *testing.T) {
	t.Run("removes_new_update_sh_and_restores_compose_on_rollback", func(t *testing.T) {
		e := newEnv(t)
		e.set("remote_compose", "services:\n  backend:\n    image: ghcr.io/hl9565/video-canvas-backend:1.1.0\n")
		e.set("remote_update", "#!/usr/bin/env bash\necho new-update\n")
		e.set("health", "backend starting\nagent healthy\nweb\n")
		out, code, st := e.run("--tag", "1.1.0", "--timeout", "1")
		if code == 0 || st["phase"] != "rollback" || st["state"] != "failed_needs_attention" {
			t.Fatalf("预期进入 rollback 流程并失败：%d %v\n%s", code, st, out)
		}
		if got := mustRead(t, filepath.Join(e.root, "docker-compose.yml")); got != "services: {}\n" {
			t.Errorf("回退后 docker-compose.yml 应恢复旧版：%q", got)
		}
		if _, err := os.Stat(filepath.Join(e.root, "update.sh")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("升级前不存在 update.sh 时回退应清理新建文件，err=%v", err)
		}
		if got := e.envValue("IMAGE_TAG"); got != "1.0.0" {
			t.Errorf("IMAGE_TAG 应退回 1.0.0：%q", got)
		}
	})

	t.Run("restores_existing_update_sh_on_pull_failure", func(t *testing.T) {
		e := newEnv(t)
		oldUpdate := "#!/usr/bin/env bash\necho old-update\n"
		if err := os.WriteFile(filepath.Join(e.root, "update.sh"), []byte(oldUpdate), 0o755); err != nil {
			t.Fatal(err)
		}
		e.set("remote_compose", "services:\n  backend:\n    image: ghcr.io/hl9565/video-canvas-backend:1.1.0\n")
		e.set("remote_update", "#!/usr/bin/env bash\necho new-update\n")
		e.set("pull_rc", "1")
		out, code, st := e.run("--tag", "1.1.0")
		if code == 0 || st["state"] != "failed" {
			t.Fatalf("拉镜像失败应中止：%d %v\n%s", code, st, out)
		}
		if got := mustRead(t, filepath.Join(e.root, "docker-compose.yml")); got != "services: {}\n" {
			t.Errorf("恢复后 docker-compose.yml 应为旧版：%q", got)
		}
		if got := mustRead(t, filepath.Join(e.root, "update.sh")); got != oldUpdate {
			t.Errorf("恢复后 update.sh 应为旧版：%q", got)
		}
	})
}
