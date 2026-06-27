"use strict";

const state = {
  gateway: "",
  token: "",
  cluster: "",
  instance: "",
  ws: null,
  reqId: 0,
  session: "webui-" + Math.random().toString(16).slice(2, 10),
  busy: false,
};

const $ = (id) => document.getElementById(id);

document.addEventListener("DOMContentLoaded", init);

async function init() {
  // 预填默认网关
  try {
    const cfg = await fetch("/api/config").then((r) => r.json());
    if (cfg.default_gateway) $("gateway").value = cfg.default_gateway;
  } catch (_) {}

  $("session-pill").textContent = "session: " + state.session;
  $("session-pill").classList.remove("hidden");

  document.querySelectorAll(".tab").forEach((tab) => {
    tab.addEventListener("click", () => {
      document.querySelectorAll(".tab").forEach((t) => t.classList.remove("active"));
      tab.classList.add("active");
      const mode = tab.dataset.mode;
      $("password-fields").classList.toggle("hidden", mode !== "password");
      $("token-fields").classList.toggle("hidden", mode !== "token");
    });
  });

  $("connect-btn").addEventListener("click", connect);
  $("refresh-targets").addEventListener("click", loadTargets);
  $("ask-form").addEventListener("submit", (e) => {
    e.preventDefault();
    sendAsk();
  });
  $("instruction").addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault();
      sendAsk();
    }
  });
}

function activeMode() {
  return document.querySelector(".tab.active").dataset.mode;
}

async function connect() {
  const gateway = $("gateway").value.trim();
  if (!gateway) return setConnectStatus("请填写 gateway 地址", "error");
  state.gateway = gateway;

  setConnectStatus("连接中…", "");
  $("connect-btn").disabled = true;

  try {
    if (activeMode() === "password") {
      const resp = await fetch("/api/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          gateway,
          username: $("username").value.trim(),
          password: $("password").value,
          name: "webui",
        }),
      });
      const data = await resp.json().catch(() => ({}));
      if (!resp.ok) throw new Error(data.error || "登录失败 (HTTP " + resp.status + ")");
      state.token = data.token;
    } else {
      const token = $("token").value.trim();
      if (!token) throw new Error("请粘贴 user token");
      state.token = token;
    }

    await loadTargets();
    setConnectStatus("已连接，选择一个实例开始", "ok");
    $("targets-card").classList.remove("hidden");
  } catch (err) {
    setConnectStatus(err.message, "error");
  } finally {
    $("connect-btn").disabled = false;
  }
}

async function loadTargets() {
  const url = "/api/targets?gateway=" + encodeURIComponent(state.gateway);
  const resp = await fetch(url, { headers: { Authorization: "Bearer " + state.token } });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || "获取 targets 失败 (HTTP " + resp.status + ")");
  renderTargets(data.targets || []);
}

function renderTargets(targets) {
  const list = $("targets-list");
  list.innerHTML = "";
  if (!targets.length) {
    list.innerHTML = '<li class="muted">当前没有在线实例</li>';
    return;
  }
  targets.forEach((t) => {
    const li = document.createElement("li");
    li.className = "target";
    if (t.cluster === state.cluster && t.instance === state.instance) li.classList.add("selected");
    const status = t.status || (t.busy ? "busy" : "idle");
    li.innerHTML =
      '<div><div class="name"><span class="dot ' + status + '"></span>' +
      escapeHtml(t.instance) + '</div>' +
      '<div class="sub">' + escapeHtml(t.cluster) + " · " + status + "</div></div>";
    li.addEventListener("click", () => selectTarget(t));
    list.appendChild(li);
  });
}

function selectTarget(t) {
  state.cluster = t.cluster;
  state.instance = t.instance;
  $("active-target").textContent = t.cluster + " / " + t.instance;
  document.querySelectorAll(".target").forEach((el) => el.classList.remove("selected"));
  // 重新渲染高亮
  loadTargets().catch(() => {});
  openRPC();
}

function openRPC() {
  if (state.ws) {
    try { state.ws.close(); } catch (_) {}
    state.ws = null;
  }
  clearStream();
  setWsState("connecting", "连接中");

  const proto = location.protocol === "https:" ? "wss" : "ws";
  const url =
    proto + "://" + location.host + "/api/rpc" +
    "?gateway=" + encodeURIComponent(state.gateway) +
    "&cluster=" + encodeURIComponent(state.cluster) +
    "&instance=" + encodeURIComponent(state.instance) +
    "&token=" + encodeURIComponent(state.token);

  const ws = new WebSocket(url);
  state.ws = ws;

  ws.onopen = () => {
    ws.send(JSON.stringify({
      jsonrpc: "2.0",
      method: "initialize",
      id: nextId(),
      params: {
        protocolVersion: "2024-11-05",
        clientInfo: { name: "doops-webui", version: "1.0" },
      },
    }));
  };
  ws.onmessage = (ev) => handleMessage(ev.data);
  ws.onerror = () => setWsState("error", "连接错误");
  ws.onclose = () => {
    setWsState("idle", "已断开");
    setComposerEnabled(false);
  };
}

let initialized = false;
function handleMessage(raw) {
  let msg;
  try { msg = JSON.parse(raw); } catch (_) { return; }

  // initialize 响应
  if (msg.result && msg.result.serverInfo && !initialized) {
    initialized = true;
    setWsState("open", "已就绪");
    setComposerEnabled(true);
    return;
  }

  if (msg.method === "notifications/message" && msg.params && typeof msg.params.data === "string") {
    renderChunk(msg.params.data);
    return;
  }

  // 最终结果
  if (msg.id !== undefined && (msg.result || msg.error)) {
    if (msg.error) {
      appendEvt("err", "✖ " + (msg.error.message || JSON.stringify(msg.error)));
    } else if (msg.result) {
      const text = extractResultText(msg.result);
      if (text) appendEvt("ai", "💬 " + text);
      if (String(msg.result.isError) === "true") appendEvt("err", "✖ 工具返回错误");
    }
    state.busy = false;
    setComposerEnabled(true);
  }
}

// renderChunk 复刻 CLI 的解析：每行可能是「纯文本前缀 + JSON 事件」
function renderChunk(chunk) {
  chunk.split("\n").forEach((line) => {
    const trimmed = line.trim();
    if (!trimmed) return;
    const idx = trimmed.indexOf("{");
    if (idx !== -1) {
      const prefix = trimmed.slice(0, idx);
      const jsonPart = trimmed.slice(idx);
      let evt;
      try { evt = JSON.parse(jsonPart); } catch (_) { evt = null; }
      if (evt) {
        if (prefix) appendEvt("raw", prefix);
        renderAgentEvent(evt);
        return;
      }
    }
    appendEvt("raw", line);
  });
}

function renderAgentEvent(evt) {
  switch (evt.type) {
    case "step_start":
      appendEvt("step", "🚀 开始新步骤…");
      break;
    case "tool_use": {
      const part = evt.part || {};
      const st = part.state || {};
      const input = st.input || {};
      const label = part.tool || "tool";
      const summary = input.command || input.description || st.title || "";
      appendEvt("tool", "🔧 [" + label + "] " + summary);
      if (st.error) appendEvt("err", "  │ " + st.error);
      else if (st.output) {
        const lines = String(st.output).trim().split("\n");
        const show = lines.length > 15 ? lines.slice(0, 5).concat("… (" + (lines.length - 10) + " 行省略)", lines.slice(-5)) : lines;
        show.forEach((l) => appendEvt("toolout", l));
      }
      break;
    }
    case "text": {
      const text = (evt.part || {}).text;
      if (text) appendEvt("ai", "💬 " + text);
      break;
    }
    case "step_finish": {
      const reason = (evt.part || {}).reason || "";
      if (reason === "end-turn" || reason === "stop") appendEvt("done", "✅ 任务完成");
      else if (reason === "tool-calls") appendEvt("toolout", "↳ 继续执行下一步…");
      else appendEvt("toolout", "↳ 步骤结束 (" + reason + ")");
      break;
    }
  }
}

function sendAsk() {
  const instruction = $("instruction").value.trim();
  if (!instruction || state.busy) return;
  if (!state.ws || state.ws.readyState !== WebSocket.OPEN) return;

  appendUser(instruction);
  $("instruction").value = "";
  state.busy = true;
  setComposerEnabled(false);

  state.ws.send(JSON.stringify({
    jsonrpc: "2.0",
    method: "tools/call",
    id: nextId(),
    params: {
      name: "doops_agent_prompt",
      arguments: { instruction, session_id: state.session },
    },
  }));
}

function extractResultText(result) {
  const content = result.content || [];
  for (const item of content) {
    if (item && item.type === "text" && item.text) return item.text;
  }
  return "";
}

/* --- UI helpers --- */
function nextId() { return ++state.reqId; }

function setConnectStatus(msg, cls) {
  const el = $("connect-status");
  el.textContent = msg;
  el.className = "status" + (cls ? " " + cls : "");
}

function setWsState(cls, label) {
  const el = $("ws-state");
  el.className = "pill state-" + cls;
  el.textContent = label;
}

function setComposerEnabled(on) {
  $("instruction").disabled = !on;
  $("send-btn").disabled = !on;
}

function clearStream() {
  $("stream").innerHTML = "";
  initialized = false;
}

function appendEvt(kind, text) {
  const div = document.createElement("div");
  div.className = "evt " + kind;
  div.textContent = text;
  $("stream").appendChild(div);
  scrollStream();
}

function appendUser(text) {
  const wrap = document.createElement("div");
  wrap.className = "msg user";
  wrap.innerHTML = '<div class="role">你</div><div class="bubble"></div>';
  wrap.querySelector(".bubble").textContent = text;
  $("stream").appendChild(wrap);
  scrollStream();
}

function scrollStream() {
  const s = $("stream");
  s.scrollTop = s.scrollHeight;
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
