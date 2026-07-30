import childProcess from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";

const successPrefix = "SYNARA_GVISOR_COMPATIBILITY_V1:";
const failureSentinel = "SYNARA_GVISOR_COMPATIBILITY_FAILED_V1";
const requiredTools = [
  "git",
  "node",
  "bun",
  "npm",
  "pnpm",
  "go",
  "rustc",
  "cargo",
  "java",
  "javac",
  "python3",
];
const versionArgs = { go: ["version"] };
const durationsMs = {};
const tools = {};
const probes = {};
const started = process.hrtime.bigint();
const root = fs.mkdtempSync(path.join(process.cwd(), ".synara-gvisor-compat-"));

function timed(label, command, args, options = {}) {
  const before = process.hrtime.bigint();
  const result = childProcess.spawnSync(command, args, {
    cwd: options.cwd || root,
    encoding: "utf8",
    timeout: 60_000,
    maxBuffer: 65_536,
    env: process.env,
  });
  durationsMs[label] = Number((process.hrtime.bigint() - before) / 1_000_000n);
  if (result.error || result.status !== 0) throw new Error(label);
  return String(result.stdout || "");
}

async function asyncTimed(label, operation) {
  const before = process.hrtime.bigint();
  await operation();
  durationsMs[label] = Number((process.hrtime.bigint() - before) / 1_000_000n);
}

function write(relative, content) {
  const target = path.join(root, relative);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, content);
  return target;
}

function delay(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

async function watchProbe() {
  const watched = path.join(root, "watch");
  fs.mkdirSync(watched);
  await new Promise((resolve, reject) => {
    let done = false;
    const watcher = fs.watch(watched, (_event, name) => {
      if (String(name) === "ready.txt" && !done) {
        done = true;
        watcher.close();
        resolve();
      }
    });
    const timer = setTimeout(() => {
      if (!done) {
        done = true;
        watcher.close();
        reject(new Error("watch"));
      }
    }, 5_000);
    setTimeout(() => fs.writeFileSync(path.join(watched, "ready.txt"), "ready"), 50);
    watcher.on("close", () => clearTimeout(timer));
  });
}

async function signalProbe() {
  const childScript = write(
    "signal-child.js",
    'process.on("SIGTERM",()=>{process.stdout.write("TERM");process.exit(0)});setInterval(()=>{},1000);',
  );
  await new Promise((resolve, reject) => {
    const child = childProcess.spawn(process.execPath, [childScript], {
      cwd: root,
      stdio: ["ignore", "pipe", "ignore"],
    });
    let output = "";
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new Error("signal-timeout"));
    }, 5_000);
    child.stdout.on("data", (chunk) => {
      output += chunk.toString("utf8");
    });
    child.on("spawn", () => setTimeout(() => child.kill("SIGTERM"), 100));
    child.on("close", (code) => {
      clearTimeout(timer);
      if (code === 0 && output === "TERM") resolve();
      else reject(new Error("signal"));
    });
  });
}

async function tcpProbe() {
  await new Promise((resolve, reject) => {
    const server = net.createServer((socket) => socket.once("data", (data) => socket.end(data)));
    const timer = setTimeout(() => {
      server.close();
      reject(new Error("tcp-timeout"));
    }, 5_000);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      if (!address || typeof address === "string") {
        reject(new Error("tcp-address"));
        return;
      }
      const client = net.createConnection({ host: "127.0.0.1", port: address.port }, () =>
        client.end("ping"),
      );
      let output = "";
      client.on("data", (chunk) => {
        output += chunk.toString("utf8");
      });
      client.on("close", () => {
        clearTimeout(timer);
        server.close();
        if (output === "ping") resolve();
        else reject(new Error("tcp"));
      });
      client.on("error", reject);
    });
    server.on("error", reject);
  });
}

try {
  const procVersion = fs.readFileSync("/proc/version", "utf8");
  if (!/gvisor/i.test(procVersion)) throw new Error("kernel");
  probes.gvisorKernel = true;

  for (const tool of requiredTools) {
    timed(`version.${tool}`, tool, versionArgs[tool] || ["--version"]);
    tools[tool] = true;
  }

  const repository = path.join(root, "repo");
  fs.mkdirSync(repository);
  timed("git.init", "git", ["init", "-q"], { cwd: repository });
  timed("git.config.email", "git", ["config", "user.email", "acceptance@synara.invalid"], {
    cwd: repository,
  });
  timed("git.config.name", "git", ["config", "user.name", "Synara Acceptance"], {
    cwd: repository,
  });
  fs.writeFileSync(path.join(repository, "tracked.txt"), "gvisor\n");
  timed("git.add", "git", ["add", "tracked.txt"], { cwd: repository });
  timed("git.commit", "git", ["commit", "-q", "-m", "probe"], { cwd: repository });
  timed("git.clone", "git", ["clone", "-q", repository, path.join(root, "clone")]);
  timed("git.checkout", "git", ["checkout", "-q", "HEAD"], {
    cwd: path.join(root, "clone"),
  });
  timed("git.status", "git", ["status", "--porcelain"], { cwd: path.join(root, "clone") });
  probes.git = true;

  write("node-probe.js", 'process.stdout.write("node-ok")');
  if (timed("compile.node", "node", [path.join(root, "node-probe.js")]) !== "node-ok") {
    throw new Error("node");
  }
  probes.node = true;

  write("bun-probe.ts", 'process.stdout.write("bun-ok")');
  if (timed("compile.bun", "bun", ["run", path.join(root, "bun-probe.ts")]) !== "bun-ok") {
    throw new Error("bun");
  }
  probes.bun = true;

  write(
    "package.json",
    JSON.stringify({
      name: "synara-gvisor-probe",
      version: "1.0.0",
      private: true,
      scripts: { probe: "node node-probe.js" },
    }),
  );
  timed("install.npm", "npm", [
    "install",
    "--offline",
    "--ignore-scripts",
    "--no-audit",
    "--no-fund",
  ]);
  timed("install.pnpm", "pnpm", ["install", "--offline", "--ignore-scripts"]);
  timed("install.bun", "bun", ["install", "--offline", "--ignore-scripts"]);
  if (!timed("package.npm", "npm", ["run", "--silent", "probe"]).includes("node-ok")) {
    throw new Error("npm");
  }
  if (!timed("package.pnpm", "pnpm", ["run", "--silent", "probe"]).includes("node-ok")) {
    throw new Error("pnpm");
  }
  if (!timed("package.bun", "bun", ["run", "--silent", "probe"]).includes("node-ok")) {
    throw new Error("bun-package");
  }
  probes.packageManagers = true;

  write("go.mod", "module synara.invalid/gvisorprobe\n\ngo 1.23\n");
  write("main.go", 'package main\nimport "fmt"\nfunc main(){fmt.Print("go-ok")}\n');
  timed("compile.go", "go", [
    "build",
    "-o",
    path.join(root, "go-probe"),
    path.join(root, "main.go"),
  ]);
  if (timed("run.go", path.join(root, "go-probe"), []) !== "go-ok") throw new Error("go");
  probes.go = true;

  write("main.rs", 'fn main(){print!("rust-ok");}\n');
  timed("compile.rust", "rustc", [path.join(root, "main.rs"), "-o", path.join(root, "rust-probe")]);
  if (timed("run.rust", path.join(root, "rust-probe"), []) !== "rust-ok") {
    throw new Error("rust");
  }
  probes.rust = true;

  const javaDirectory = path.join(root, "java");
  fs.mkdirSync(javaDirectory);
  write(
    "java/Probe.java",
    'public class Probe { public static void main(String[] a){ System.out.print("java-ok"); } }\n',
  );
  timed("compile.java", "javac", [path.join(javaDirectory, "Probe.java")], { cwd: javaDirectory });
  if (
    timed("run.java", "java", ["-cp", javaDirectory, "Probe"], { cwd: javaDirectory }) !== "java-ok"
  ) {
    throw new Error("java");
  }
  probes.java = true;

  write("python-probe.py", 'print("python-ok",end="")\n');
  if (timed("run.python", "python3", [path.join(root, "python-probe.py")]) !== "python-ok") {
    throw new Error("python");
  }
  probes.python = true;

  const ptyScript = write(
    "pty-probe.py",
    'import os,pty\nstatus=pty.spawn(["/bin/sh","-lc","printf pty-ok"])\nraise SystemExit(os.waitstatus_to_exitcode(status))\n',
  );
  if (!timed("pty", "python3", [ptyScript]).includes("pty-ok")) throw new Error("pty");
  probes.pty = true;

  const metadataStarted = process.hrtime.bigint();
  const metadata = write("metadata.txt", "before");
  fs.chmodSync(metadata, 0o600);
  const descriptor = fs.openSync(metadata, "r+");
  fs.fsyncSync(descriptor);
  fs.closeSync(descriptor);
  const renamed = path.join(root, "metadata-renamed.txt");
  fs.renameSync(metadata, renamed);
  const stat = fs.statSync(renamed);
  if ((stat.mode & 0o777) !== 0o600 || stat.size !== 6) throw new Error("metadata");
  durationsMs["file.metadata"] = Number((process.hrtime.bigint() - metadataStarted) / 1_000_000n);
  probes.fileMetadata = true;

  await asyncTimed("file.watch", watchProbe);
  probes.fileWatch = true;
  await asyncTimed("process.signal", signalProbe);
  probes.signal = true;
  await asyncTimed("network.loopbackTcp", tcpProbe);
  probes.loopbackTcp = true;
  await delay(1);

  durationsMs.total = Number((process.hrtime.bigint() - started) / 1_000_000n);
  const result = {
    schemaVersion: 1,
    runtime: "gvisor",
    tools,
    probes,
    durationsMs,
    maxRssKiB: process.resourceUsage().maxRSS,
  };
  process.stdout.write(
    successPrefix + Buffer.from(JSON.stringify(result), "utf8").toString("base64url"),
  );
} catch (_error) {
  process.stdout.write(`${failureSentinel}\n`);
  process.exitCode = 1;
} finally {
  fs.rmSync(root, { recursive: true, force: true });
}
