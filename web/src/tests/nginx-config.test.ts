import { describe, expect, test } from "bun:test";
import { type ChildProcess, spawn, spawnSync } from "node:child_process";
import {
  accessSync,
  constants,
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import {
  createConnection,
  createServer as createNetServer,
  type Server as NetServer,
  type Socket,
} from "node:net";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";

const WEB_ROOT = join(import.meta.dir, "..", "..");
const NGINX_CONF_PATH = join(WEB_ROOT, "nginx.conf");
const DOCKERFILE_PATH = join(WEB_ROOT, "Dockerfile");

interface NginxDirective {
  name: string;
  args: string[];
  raw: string;
}

interface NginxBlock {
  name: string;
  args: string[];
  directives: NginxDirective[];
  children: NginxBlock[];
}

interface LocationRule {
  modifier: "=" | "^~" | "~" | "~*" | "";
  pattern: string;
  block: NginxBlock;
}

/** 去掉不在引号内的 # 注释 */
function stripNginxComments(source: string): string {
  let out = "";
  let quote: '"' | "'" | null = null;
  for (let i = 0; i < source.length; i += 1) {
    const ch = source[i]!;
    if (quote) {
      if (ch === "\\" && i + 1 < source.length) {
        out += ch + source[i + 1]!;
        i += 1;
        continue;
      }
      if (ch === quote) quote = null;
      out += ch;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
      out += ch;
      continue;
    }
    if (ch === "#") {
      while (i < source.length && source[i] !== "\n") i += 1;
      if (i < source.length) out += "\n";
      continue;
    }
    out += ch;
  }
  return out;
}

/** 分割空白分隔的参数，保留引号内整体（支持反斜杠转义） */
function splitNginxTokens(raw: string): string[] {
  const tokens: string[] = [];
  let cur = "";
  let quote: '"' | "'" | null = null;
  let hasToken = false;

  for (let i = 0; i < raw.length; i += 1) {
    const ch = raw[i]!;
    if (quote) {
      if (ch === "\\" && i + 1 < raw.length) {
        cur += raw[i + 1]!;
        i += 1;
        continue;
      }
      if (ch === quote) {
        quote = null;
        continue;
      }
      cur += ch;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
      hasToken = true;
      continue;
    }
    if (/\s/.test(ch)) {
      if (hasToken) {
        tokens.push(cur);
        cur = "";
        hasToken = false;
      }
      continue;
    }
    cur += ch;
    hasToken = true;
  }
  if (hasToken) tokens.push(cur);
  return tokens;
}

/** 递归解析 Nginx 配置块与指令 */
function parseNginxConfig(source: string): {
  directives: NginxDirective[];
  blocks: NginxBlock[];
} {
  const cleaned = stripNginxComments(source);
  let pos = 0;

  function parseScope(): { directives: NginxDirective[]; blocks: NginxBlock[] } {
    const directives: NginxDirective[] = [];
    const blocks: NginxBlock[] = [];
    let buf = "";
    let quote: '"' | "'" | null = null;

    while (pos < cleaned.length) {
      const ch = cleaned[pos]!;
      pos += 1;

      if (quote) {
        buf += ch;
        if (ch === "\\" && pos < cleaned.length) {
          buf += cleaned[pos]!;
          pos += 1;
        } else if (ch === quote) {
          quote = null;
        }
        continue;
      }
      if (ch === '"' || ch === "'") {
        quote = ch;
        buf += ch;
        continue;
      }
      if (ch === "}") {
        const trailing = buf.trim();
        if (trailing) {
          throw new Error(`Unexpected trailing tokens before '}': ${trailing}`);
        }
        return { directives, blocks };
      }
      if (ch === "{") {
        const tokens = splitNginxTokens(buf);
        buf = "";
        if (tokens.length === 0) {
          throw new Error("Encountered anonymous '{' block without directive name");
        }
        const [name, ...args] = tokens;
        const inner = parseScope();
        blocks.push({
          name: name!,
          args,
          directives: inner.directives,
          children: inner.blocks,
        });
        continue;
      }
      if (ch === ";") {
        const raw = buf.trim();
        buf = "";
        if (!raw) continue;
        const tokens = splitNginxTokens(raw);
        const [name, ...args] = tokens;
        directives.push({ name: name!, args, raw });
        continue;
      }
      buf += ch;
    }

    const trailing = buf.trim();
    if (trailing) {
      throw new Error(`Unterminated Nginx directive: ${trailing}`);
    }
    return { directives, blocks };
  }

  return parseScope();
}

/** 提取 server 块下所有 location 规则 */
function extractLocations(server: NginxBlock): LocationRule[] {
  return server.children
    .filter((child) => child.name === "location")
    .map((block) => {
      const [first, second] = block.args;
      if (first === "=" || first === "^~" || first === "~" || first === "~*") {
        return { modifier: first, pattern: second ?? "", block };
      }
      return { modifier: "", pattern: first ?? "", block };
    });
}

/** 模拟 Nginx 对 URI 路径的百分号解码、斜杠合并与点段归约 */
function normalizeNginxPath(rawTarget: string): string {
  const pathOnly = rawTarget.split(/[?#]/, 1)[0] ?? "/";
  const decoded = pathOnly.replace(/%([0-9a-fA-F]{2})/g, (_, hex: string) =>
    String.fromCharCode(Number.parseInt(hex, 16)),
  );
  const merged = decoded.replace(/\/{2,}/g, "/");
  const segments: string[] = [];
  for (const part of merged.split("/")) {
    if (!part || part === ".") continue;
    if (part === "..") {
      segments.pop();
      continue;
    }
    segments.push(part);
  }
  const trailingSlash = merged.endsWith("/") && segments.length > 0 ? "/" : "";
  return `/${segments.join("/")}${trailingSlash}`;
}

/** 模拟 Nginx location 选择顺序：= -> 最长 ^~ -> 按序正则 (~ / ~*) -> 最长普通前缀 */
function matchNginxLocation(locations: LocationRule[], rawTarget: string): LocationRule | null {
  const uri = normalizeNginxPath(rawTarget);

  for (const loc of locations) {
    if (loc.modifier === "=" && uri === loc.pattern) return loc;
  }

  let bestPrefix: LocationRule | null = null;
  for (const loc of locations) {
    if (loc.modifier !== "" && loc.modifier !== "^~") continue;
    if (!uri.startsWith(loc.pattern)) continue;
    if (!bestPrefix || loc.pattern.length > bestPrefix.pattern.length) {
      bestPrefix = loc;
    }
  }
  if (bestPrefix?.modifier === "^~") return bestPrefix;

  for (const loc of locations) {
    if (loc.modifier !== "~" && loc.modifier !== "~*") continue;
    const flags = loc.modifier === "~*" ? "i" : "";
    if (new RegExp(loc.pattern, flags).test(uri)) return loc;
  }

  return bestPrefix;
}

function directiveArgs(block: NginxBlock, name: string): string[][] {
  return block.directives.filter((d) => d.name === name).map((d) => d.args);
}

function isExecutable(filePath: string): boolean {
  if (!existsSync(filePath)) return false;
  try {
    accessSync(filePath, constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

function findNginxBinary(): string | null {
  const pathDirs = (process.env.PATH ?? "").split(delimiter).filter(Boolean);
  const candidates = [
    ...pathDirs.map((dir) => join(dir, "nginx")),
    "/opt/homebrew/bin/nginx",
    "/opt/homebrew/sbin/nginx",
    "/usr/sbin/nginx",
    "/usr/local/sbin/nginx",
    "/usr/local/bin/nginx",
  ];
  for (const candidate of candidates) {
    if (isExecutable(candidate)) return candidate;
  }
  return null;
}

/** 强制模式（CI=true 或 REQUIRE_NGINX=1）缺 nginx 时立即失败，本地缺 nginx 时输出醒目警告 */
function resolveNginxBinaryOrReport(
  nginxBin: string | null,
  env: Record<string, string | undefined>,
  warn: (message: string) => void = console.warn,
): string | null {
  if (nginxBin) return nginxBin;
  if (env.CI === "true" || env.REQUIRE_NGINX === "1") {
    throw new Error(
      "nginx binary is required when CI=true or REQUIRE_NGINX=1; cannot skip live nginx -t and proxy log-isolation tests.",
    );
  }
  warn(
    "[nginx-config.test] WARNING: nginx binary not found locally; live nginx -t and proxy log-isolation checks did NOT run (set REQUIRE_NGINX=1 to enforce).",
  );
  return null;
}

async function allocatePort(): Promise<number> {
  return await new Promise((resolve, reject) => {
    const server = createNetServer();
    server.unref();
    server.on("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") {
        server.close(() => reject(new Error("Failed to allocate ephemeral port")));
        return;
      }
      const { port } = addr;
      server.close((err) => (err ? reject(err) : resolve(port)));
    });
  });
}

async function stopProcess(proc: ChildProcess): Promise<void> {
  if (proc.exitCode !== null || proc.signalCode !== null) return;
  await new Promise<void>((resolve) => {
    const timer = setTimeout(() => {
      proc.kill("SIGKILL");
    }, 2000);
    proc.once("exit", () => {
      clearTimeout(timer);
      resolve();
    });
    proc.kill("SIGTERM");
  });
}

describe("web/nginx.conf 预览流日志与代理不变量", () => {
  const nginxConf = readFileSync(NGINX_CONF_PATH, "utf8");
  const dockerfile = readFileSync(DOCKERFILE_PATH, "utf8");
  const parsed = parseNginxConfig(nginxConf);
  const mapBlock = parsed.blocks.find((b) => b.name === "map");
  const server = parsed.blocks.find((b) => b.name === "server");
  const locations = server ? extractLocations(server) : [];

  test("挂载于 /etc/nginx/conf.d/default.conf，声明 ticket/token 脱敏日志格式且仅单 server {}", () => {
    expect(dockerfile).toContain("COPY nginx.conf /etc/nginx/conf.d/default.conf");
    expect(parsed.blocks.filter((b) => b.name === "server")).toHaveLength(1);
    expect(mapBlock?.args).toEqual(["$request", "$vc_safe_request"]);
    expect(parsed.directives.map((d) => d.name)).toEqual(["log_format"]);
    expect(parsed.directives[0]?.args[0]).toBe("vc_safe");
    expect(directiveArgs(server!, "access_log")).toEqual([
      ["/var/log/nginx/access.log", "vc_safe"],
    ]);
    expect(directiveArgs(server!, "error_log")).toEqual([["/var/log/nginx/error.log", "warn"]]);
    expect(nginxConf).not.toMatch(/\bhttp\s*\{/);
    expect(nginxConf).not.toMatch(/\bevents\s*\{/);
  });

  test("预览流 location 禁用 access_log、隔离含 URI 的 error_log 并保留 Range/SSE 代理头", () => {
    expect(server).toBeDefined();
    const streamIndex = locations.findIndex(
      (loc) => (loc.modifier === "~" || loc.modifier === "~*") && loc.pattern.includes("stream"),
    );
    const apiIndex = locations.findIndex((loc) => loc.modifier === "" && loc.pattern === "/api/");
    expect(streamIndex).toBeGreaterThanOrEqual(0);
    expect(apiIndex).toBeGreaterThan(streamIndex);

    const streamLoc = locations[streamIndex]!;
    expect(directiveArgs(streamLoc.block, "access_log")).toEqual([["off"]]);

    const errorLogs = directiveArgs(streamLoc.block, "error_log");
    expect(errorLogs.length).toBeGreaterThan(0);
    for (const args of errorLogs) {
      // `error_log off` 在 Nginx 中会创建名为 off 的文件，必须指向 /dev/null 且阈值不低于 crit
      expect(args[0]).toBe("/dev/null");
      expect(["crit", "alert", "emerg"]).toContain(args[1]);
    }

    expect(directiveArgs(streamLoc.block, "proxy_buffering")).toEqual([["off"]]);
    expect(directiveArgs(streamLoc.block, "proxy_request_buffering")).toEqual([["off"]]);
    expect(directiveArgs(streamLoc.block, "proxy_send_timeout")).toEqual([["600s"]]);
    expect(directiveArgs(streamLoc.block, "proxy_read_timeout")).toEqual([["600s"]]);
    expect(directiveArgs(streamLoc.block, "proxy_pass")).toEqual([["http://backend:8080"]]);
    expect(directiveArgs(streamLoc.block, "proxy_http_version")).toEqual([["1.1"]]);
    expect(directiveArgs(streamLoc.block, "proxy_set_header")).toEqual([
      ["Range", "$http_range"],
      ["If-Range", "$http_if_range"],
      ["Host", "$host"],
      ["X-Real-IP", "$remote_addr"],
      ["X-Forwarded-For", "$proxy_add_x_forwarded_for"],
      ["X-Forwarded-Proto", "$forwarded_proto"],
    ]);

    const apiLoc = locations[apiIndex]!;
    expect(directiveArgs(apiLoc.block, "client_max_body_size")).toEqual([["110m"]]);
    expect(directiveArgs(apiLoc.block, "proxy_request_buffering")).toEqual([["off"]]);
    expect(directiveArgs(apiLoc.block, "proxy_pass")).toEqual([["http://backend:8080"]]);
    expect(directiveArgs(apiLoc.block, "proxy_set_header")).toContainEqual([
      "X-Forwarded-Proto",
      "$forwarded_proto",
    ]);

    const filesLoc = locations.find((loc) => loc.pattern === "/files/");
    expect(filesLoc).toBeDefined();
    expect(directiveArgs(filesLoc!.block, "proxy_pass")).toEqual([["http://backend:8080"]]);

    const wsLoc = locations.find(
      (loc) => loc.modifier === "~*" && loc.pattern === "^/+api/+v1/+ws(?:;[^/]*)?/?$",
    );
    expect(wsLoc).toBeDefined();
    expect(directiveArgs(wsLoc!.block, "access_log")).toEqual([["off"]]);
    expect(directiveArgs(wsLoc!.block, "error_log")).toEqual([["/dev/null", "emerg"]]);
    expect(directiveArgs(wsLoc!.block, "proxy_pass")).toEqual([["http://backend:8080"]]);
    expect(directiveArgs(wsLoc!.block, "proxy_http_version")).toEqual([["1.1"]]);
    expect(directiveArgs(wsLoc!.block, "proxy_set_header")).toContainEqual([
      "Upgrade",
      "$http_upgrade",
    ]);
    expect(directiveArgs(wsLoc!.block, "proxy_set_header")).toContainEqual([
      "Connection",
      "upgrade",
    ]);
    expect(directiveArgs(wsLoc!.block, "proxy_set_header")).toContainEqual([
      "X-Real-IP",
      "$remote_addr",
    ]);
    expect(directiveArgs(wsLoc!.block, "proxy_set_header")).toContainEqual([
      "X-Forwarded-Proto",
      "$forwarded_proto",
    ]);

    const rootLoc = locations.find((loc) => loc.pattern === "/");
    expect(rootLoc).toBeDefined();
    expect(directiveArgs(rootLoc!.block, "try_files")).toEqual([["$uri", "$uri/", "/index.html"]]);
  });

  test("预览流路径变体全部命中隔离 location，非流路由保持原归属", () => {
    const streamVariants = [
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef/stream?token=secret",
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef/stream/?token=secret",
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef/stream///?token=secret",
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef/stream/chunk.mp4?token=secret",
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef/stream.mp4?token=secret",
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef/stream;v=1?token=secret",
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef/STREAM?token=secret",
      "/API/v1/dramas/d1/episodes/1/preview/0123456789abcdef/stream/?token=secret",
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef//stream?token=secret",
      "/api/v1/dramas//episodes/1/preview//stream/?token=secret",
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef/./stream?token=secret",
      "/api/v1/dramas/a%2Fb/episodes/1/preview/0123456789abcdef/stream?token=secret",
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef/%73tream?token=secret",
      "/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef/stream%3Ftoken=secret",
    ];
    for (const path of streamVariants) {
      const matched = matchNginxLocation(locations, path);
      expect(matched?.pattern, path).toContain("stream");
      expect(directiveArgs(matched!.block, "access_log")).toEqual([["off"]]);
      expect(directiveArgs(matched!.block, "error_log")[0]?.[0]).toBe("/dev/null");
    }

    const nonStreamRoutes: Array<[string, string]> = [
      ["/api/v1/dramas/d1/episodes/1/preview", "/api/"],
      ["/api/v1/dramas/d1/episodes/1/preview/0123456789abcdef", "/api/"],
      ["/api/v1/dramas/d1/episodes/1/manifest", "/api/"],
      ["/api/v1/dramas/d1/episodes/1/segments", "/api/"],
      ["/api/v1/ws?ticket=t1", "^/+api/+v1/+ws(?:;[^/]*)?/?$"],
      ["/api/v1/ws/?ticket=t1", "^/+api/+v1/+ws(?:;[^/]*)?/?$"],
      ["/api/v1/WS/?ticket=t1", "^/+api/+v1/+ws(?:;[^/]*)?/?$"],
      ["/api/v1//ws?ticket=t1", "^/+api/+v1/+ws(?:;[^/]*)?/?$"],
      ["/api/v1/ws/ticket", "/api/"],
      ["/files/asset.mp4", "/files/"],
      ["/drama/123", "/"],
    ];
    for (const [path, expectedPattern] of nonStreamRoutes) {
      const matched = matchNginxLocation(locations, path);
      expect(matched?.pattern, path).toBe(expectedPattern);
    }
  });

  test("CI=true 或 REQUIRE_NGINX=1 缺 nginx 必须明确失败，本地无强制时显式告警", () => {
    expect(() => resolveNginxBinaryOrReport(null, { CI: "true" }, () => {})).toThrow(
      /REQUIRE_NGINX=1/,
    );
    expect(() => resolveNginxBinaryOrReport(null, { REQUIRE_NGINX: "1" }, () => {})).toThrow(
      /CI=true/,
    );

    const warnings: string[] = [];
    expect(resolveNginxBinaryOrReport(null, {}, (msg) => warnings.push(msg))).toBeNull();
    expect(warnings).toHaveLength(1);
    expect(warnings[0]).toContain("did NOT run");
    expect(resolveNginxBinaryOrReport("/opt/homebrew/bin/nginx", { REQUIRE_NGINX: "1" })).toBe(
      "/opt/homebrew/bin/nginx",
    );
  });

  test("可用 nginx 时校验 nginx -t 与预览流 token、WebSocket ticket 日志隔离；缺失按环境策略处理", async () => {
    const nginxBin = resolveNginxBinaryOrReport(findNginxBinary(), process.env);
    if (!nginxBin) return;

    const workDir = mkdtempSync(join(tmpdir(), "vc-nginx-test-"));
    let proc: ChildProcess | null = null;
    let upstream: NetServer | null = null;
    const upstreamSockets = new Set<Socket>();
    const closeUpstream = async () => {
      if (!upstream) return;
      const srv = upstream;
      upstream = null;
      for (const s of upstreamSockets) s.destroy();
      upstreamSockets.clear();
      await new Promise<void>((resolve) => srv.close(() => resolve()));
    };
    try {
      const nginxPort = await allocatePort();
      const accessLog = join(workDir, "access.log");
      const errorLog = join(workDir, "error.log");
      const startupErrorLog = join(workDir, "startup-error.log");
      const pidFile = join(workDir, "nginx.pid");
      const testSiteConf = join(workDir, "default.conf");
      const mainConf = join(workDir, "nginx.conf");
      const spaHTML = "<!doctype html><title>Video Canvas SPA</title>";
      writeFileSync(join(workDir, "index.html"), spaHTML, "utf8");
      let sawRangeHeaders = false;

      upstream = createNetServer((socket) => {
        upstreamSockets.add(socket);
        socket.on("close", () => upstreamSockets.delete(socket));
        let buf = "";
        socket.setEncoding("utf8");
        socket.on("data", (chunk) => {
          buf += chunk;
          const endIdx = buf.indexOf("\r\n\r\n");
          if (endIdx < 0) return;
          const lines = buf.slice(0, endIdx).split("\r\n");
          const reqTarget = lines[0]?.split(" ")[1] ?? "/";
          const hdrs = new Map<string, string>();
          for (const line of lines.slice(1)) {
            const sep = line.indexOf(":");
            if (sep > 0) hdrs.set(line.slice(0, sep).toLowerCase(), line.slice(sep + 1).trim());
          }
          const url = new URL(reqTarget, "http://127.0.0.1");
          const proto = hdrs.get("x-forwarded-proto") ?? "";
          if (hdrs.get("upgrade")?.toLowerCase() === "websocket") {
            socket.write(
              [
                "HTTP/1.1 101 Switching Protocols",
                "Upgrade: websocket",
                "Connection: Upgrade",
                "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=",
                `X-Test-Upgrade-Proto: ${proto}`,
                "",
                "",
              ].join("\r\n"),
            );
            return;
          }
          const writeResp = (status: string, extra: Record<string, string>, body: string) => {
            const headLines = [
              `HTTP/1.1 ${status}`,
              `X-Test-Upstream-Proto: ${proto}`,
              `Content-Length: ${Buffer.byteLength(body)}`,
              "Connection: close",
              ...Object.entries(extra).map(([k, v]) => `${k}: ${v}`),
              "",
              "",
            ];
            socket.end(headLines.join("\r\n") + body);
          };
          if (url.pathname.toLowerCase().includes("/stream")) {
            const token = url.searchParams.get("token");
            if (token === "SECRET_FORBIDDEN") return writeResp("403 Forbidden", {}, "");
            if (token === "SECRET_REJECTED") return writeResp("401 Unauthorized", {}, "");
            if (hdrs.get("range") === "bytes=0-3") {
              sawRangeHeaders =
                hdrs.get("if-range") === '"etag-1"' &&
                Boolean(hdrs.get("x-real-ip")) &&
                Boolean(hdrs.get("x-forwarded-for")) &&
                proto === "http";
              return writeResp(
                "206 Partial Content",
                {
                  "Content-Range": "bytes 0-3/8",
                  "Accept-Ranges": "bytes",
                  "Content-Type": "video/mp4",
                },
                "mp4!",
              );
            }
            return writeResp("200 OK", {}, "mp4-full");
          }
          return writeResp("200 OK", { "Content-Type": "application/json" }, '{"status":"ready"}');
        });
      });

      await new Promise<void>((resolve) => upstream!.listen(0, "127.0.0.1", () => resolve()));
      const upstreamAddr = upstream.address();
      if (!upstreamAddr || typeof upstreamAddr === "string") {
        throw new Error("Failed to bind mock upstream server");
      }

      const rewrittenServer = nginxConf
        .replace(/listen\s+80\s*;/, `listen 127.0.0.1:${nginxPort};`)
        .replace("root /usr/share/nginx/html;", `root ${workDir};`)
        .replace(
          "access_log /var/log/nginx/access.log vc_safe;",
          `access_log ${accessLog} vc_safe;`,
        )
        .replace("error_log /var/log/nginx/error.log warn;", `error_log ${errorLog} warn;`)
        .replaceAll(
          "proxy_pass http://backend:8080;",
          `proxy_pass http://127.0.0.1:${upstreamAddr.port};`,
        );
      writeFileSync(testSiteConf, rewrittenServer, "utf8");

      writeFileSync(
        mainConf,
        [
          "worker_processes 1;",
          `pid ${pidFile};`,
          `error_log ${errorLog} info;`,
          "events { worker_connections 128; }",
          "http {",
          `  access_log ${accessLog};`,
          `  client_body_temp_path ${join(workDir, "body")};`,
          `  proxy_temp_path ${join(workDir, "proxy")};`,
          `  fastcgi_temp_path ${join(workDir, "fastcgi")};`,
          `  uwsgi_temp_path ${join(workDir, "uwsgi")};`,
          `  scgi_temp_path ${join(workDir, "scgi")};`,
          `  include ${testSiteConf};`,
          "}",
          "",
        ].join("\n"),
        "utf8",
      );

      const syntaxCheck = spawnSync(
        nginxBin,
        ["-e", startupErrorLog, "-t", "-p", workDir, "-c", mainConf],
        { encoding: "utf8" },
      );
      expect(syntaxCheck.status, syntaxCheck.stderr).toBe(0);

      proc = spawn(nginxBin, [
        "-e",
        startupErrorLog,
        "-p",
        workDir,
        "-c",
        mainConf,
        "-g",
        "daemon off;",
      ]);

      const base = `http://127.0.0.1:${nginxPort}`;
      let ready = false;
      for (let attempt = 0; attempt < 40; attempt += 1) {
        try {
          const probe = await fetch(`${base}/api/v1/dramas/d1/episodes/1/preview/tkt1`);
          if (probe.status === 200) {
            ready = true;
            break;
          }
        } catch {
          await delay(50);
        }
      }
      expect(ready).toBe(true);

      for (const path of ["/", "/drama/123"]) {
        const spa = await fetch(`${base}${path}`, { headers: { "X-Forwarded-Proto": "https" } });
        expect(spa.status, path).toBe(200);
        expect(await spa.text(), path).toBe(spaHTML);
      }

      const apiPath = "/api/v1/dramas/d1/episodes/1/preview/tkt1";
      for (const path of [apiPath, `${apiPath}/stream`]) {
        const direct = await fetch(`${base}${path}`);
        expect(direct.status, path).toBe(200);
        expect(direct.headers.get("X-Test-Upstream-Proto"), path).toBe("http");
        await direct.text();
      }

      const okRes = await fetch(
        `${base}/api/v1/dramas/d1/episodes/1/preview/tkt1/stream?token=SECRET_OK_206`,
        { headers: { Range: "bytes=0-3", "If-Range": '"etag-1"' } },
      );
      expect(okRes.status).toBe(206);
      expect(okRes.headers.get("content-range")).toBe("bytes 0-3/8");
      expect(await okRes.text()).toBe("mp4!");
      expect(sawRangeHeaders).toBe(true);

      for (const variantPath of [
        "/api/v1/dramas/d1/episodes/1/preview/tkt1/stream/?token=SECRET_SLASH",
        "/api/v1/dramas/d1/episodes/1/preview/tkt1/stream/part?token=SECRET_SUBPATH",
        "/api/v1/dramas/d1/episodes/1/preview/tkt1/STREAM/?token=SECRET_CASE",
        "/api/v1/dramas/d1/episodes/1/preview/tkt1//stream?token=SECRET_DOUBLE_SLASH",
        "/api/v1/dramas/a%2Fb/episodes/1/preview/tkt1/stream?token=SECRET_ENCODED_SLASH",
        "/api/v1/dramas/d1/episodes/1/preview/tkt1/stream.mp4?token=SECRET_EXT",
        "/api/v1/dramas/d1/episodes/1/preview/tkt1/stream;v=1?token=SECRET_SEMI",
      ]) {
        const variantRes = await fetch(`${base}${variantPath}`, { redirect: "manual" });
        expect(variantRes.status, variantPath).toBe(200);
      }

      const unauthRes = await fetch(
        `${base}/api/v1/dramas/d1/episodes/1/preview/tkt1/stream?token=SECRET_REJECTED`,
      );
      expect(unauthRes.status).toBe(401);

      const forbiddenRes = await fetch(
        `${base}/api/v1/dramas/d1/episodes/1/preview/tkt1/stream?token=SECRET_FORBIDDEN`,
      );
      expect(forbiddenRes.status).toBe(403);

      for (const path of [apiPath, `${apiPath}/stream`]) {
        const forwarded = await fetch(`${base}${path}`, {
          headers: { "X-Forwarded-Proto": "https" },
        });
        expect(forwarded.status, path).toBe(200);
        expect(forwarded.headers.get("X-Test-Upstream-Proto"), path).toBe("https");
        await forwarded.text();
      }

      const wsPath = "/api/v1/ws";
      const wsOK = await fetch(`${base}${wsPath}?ticket=SECRET_WS_TICKET`);
      expect(wsOK.status).toBe(200);
      expect(await wsOK.json()).toEqual({ status: "ready" });

      const rawRequest = (payload: string) =>
        new Promise<string>((resolve, reject) => {
          const sock = createConnection({ host: "127.0.0.1", port: nginxPort }, () => {
            sock.write(payload);
          });
          let data = "";
          let settled = false;
          const finish = () => {
            if (settled) return;
            settled = true;
            sock.destroy();
            resolve(data);
          };
          sock.setEncoding("utf8");
          sock.on("data", (chunk) => {
            data += chunk;
            if (data.includes("\r\n\r\n")) finish();
          });
          sock.on("end", finish);
          sock.on("error", (err) => {
            if (!settled) {
              settled = true;
              reject(err);
            }
          });
        });

      // 真实 WebSocket 101 握手：保留 Upgrade / Connection 与外层 X-Forwarded-Proto，且不落日志
      const wsUpgradeRaw = await rawRequest(
        [
          `GET ${wsPath}?ticket=SECRET_WS_UPGRADE_101 HTTP/1.1`,
          "Host: canvas.example.test",
          "Upgrade: websocket",
          "Connection: Upgrade",
          "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==",
          "Sec-WebSocket-Version: 13",
          "X-Forwarded-Proto: https",
          "",
          "",
        ].join("\r\n"),
      );
      expect(wsUpgradeRaw).toContain("101 Switching Protocols");
      expect(wsUpgradeRaw).toContain("X-Test-Upgrade-Proto: https");

      // 进入 location 之前被 400 拒绝（超长请求头 > 8KB），覆盖字面与百分号编码参数名：必须脱敏不得泄露未消费凭据
      const hugeHeader = "a".repeat(12 * 1024);
      for (const badTarget of [
        `${wsPath}?ticket=SECRET_WS_PRELOC_400`,
        `${wsPath}?%74icket=SECRET_WS_PCT_400`,
        "/api/v1/dramas/d1/episodes/1/preview/tkt1/stream?token=SECRET_TOKEN_PRELOC_400",
        "/api/v1/dramas/d1/episodes/1/preview/tkt1/stream?t%6fken=SECRET_TOKEN_PCT_400",
      ]) {
        const preLoc400 = await rawRequest(
          [
            `GET ${badTarget} HTTP/1.1`,
            "Host: canvas.example.test",
            `X-Huge: ${hugeHeader}`,
            "",
            "",
          ].join("\r\n"),
        );
        expect(preLoc400, badTarget).toMatch(/^HTTP\/1\.1 400 /);
      }

      // POST /api/v1/ws/ticket 签发接口与普通查询请求不被 /api/v1/ws 吞掉，且查询值被脱敏
      const issueTicket = await fetch(`${base}/api/v1/ws/ticket`, { method: "POST" });
      expect(issueTicket.status).toBe(200);
      await issueTicket.text();
      const normalQuery = await fetch(`${base}/api/v1/health?tab=overview`);
      expect(normalQuery.status).toBe(200);
      await normalQuery.text();

      await closeUpstream();

      const failCanonical = await fetch(
        `${base}/api/v1/dramas/d1/episodes/1/preview/tkt1/stream?token=SECRET_UPSTREAM_502`,
      );
      expect(failCanonical.status).toBe(502);

      const failSlash = await fetch(
        `${base}/api/v1/dramas/d1/episodes/1/preview/tkt1/stream/?token=SECRET_SLASH_502`,
      );
      expect(failSlash.status).toBe(502);

      const failApi = await fetch(`${base}/api/v1/dramas/d1/episodes/1/preview/tkt1`);
      expect(failApi.status).toBe(502);

      const failWS = await fetch(`${base}${wsPath}?ticket=SECRET_WS_502`);
      expect(failWS.status).toBe(502);
      await failWS.text();

      const failWSSlash = await fetch(`${base}${wsPath}/?ticket=SECRET_WS_SLASH_502`);
      expect(failWSSlash.status).toBe(502);
      await failWSSlash.text();

      const failWSDouble = await fetch(`${base}/api/v1//ws/?ticket=SECRET_WS_DBL_502`);
      expect(failWSDouble.status).toBe(502);
      await failWSDouble.text();

      await delay(100);
      const accessText = existsSync(accessLog) ? readFileSync(accessLog, "utf8") : "";
      const errorText = existsSync(errorLog) ? readFileSync(errorLog, "utf8") : "";

      expect(accessText).toContain("/api/v1/dramas/d1/episodes/1/preview/tkt1");
      expect(accessText).toContain('POST /api/v1/ws/ticket HTTP/1.1" 200');
      expect(accessText).toContain('GET /api/v1/health?[redacted] HTTP/1.1" 200');
      expect(accessText).toContain('GET /api/v1/ws?[redacted] HTTP/1.1" 400');
      expect(errorText).toContain("/api/v1/dramas/d1/episodes/1/preview/tkt1");
      expect(errorText).not.toContain(wsPath);
      expect(accessText).not.toContain("SECRET_WS_");
      expect(errorText).not.toContain("SECRET_WS_");
      expect(accessText).not.toContain("SECRET_TOKEN_");
      expect(errorText).not.toContain("SECRET_TOKEN_");
      expect(accessText).not.toContain("SECRET_");
      expect(errorText).not.toContain("SECRET_");
      expect(errorText).not.toContain("/stream");
    } finally {
      await closeUpstream();
      if (proc) await stopProcess(proc);
      rmSync(workDir, { recursive: true, force: true });
    }
  });
});
