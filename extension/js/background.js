/**
 * HLSink background（MV3 Service Worker）。
 *
 * 职责：
 *   1. 汇总各 tab 的嗅探结果（HLS / DASH）：content-script 上报的「页面侧请求头」与
 *      webRequest 抓到的「真实网络请求头」分开存放（pageHeaders / networkHeaders），互不覆盖；
 *   2. 对外（popup）提供合并后的 headers，保证发送契约稳定；
 *   3. 用 chrome.alarms 周期清理过期条目，并在清理后同步刷新受影响 tab 的角标；
 *   4. 转发下载任务到本地下载器，校验 HTTP 状态与响应体，带超时避免假死。
 */

"use strict";

importScripts("common.js");

const MC = globalThis.MiaocatchCommon;

const APP_URL = "http://127.0.0.1:18752";
/** 每个 tab 最多保留的捕获条目数 */
const MAX_ITEMS_PER_TAB = 50;
/** 条目存活时间 */
const ITEM_TTL = 30 * 60 * 1000;
/** 单次任务请求 / ping 的超时时间（毫秒） */
const SEND_TIMEOUT_MS = 8000;
const PING_TIMEOUT_MS = 3000;
/** 标题最长保留长度 */
const MAX_TITLE_LENGTH = 200;
const MAX_CONTENT_LENGTH = MC.MAX_CONTENT_LENGTH;

/** 周期清理 alarm */
const PRUNE_ALARM = "mc-prune";
const PRUNE_PERIOD_MINUTES = 5;

/** 注入脚本的 id */
const SCRIPT_IDS = ["miaocatch-bridge", "miaocatch-inject"];

/**
 * tabId -> {
 *   title: string,
 *   items: Map<url, {
 *     url: string,
 *     kind: string,            // "hls" / "dash"
 *     pageHeaders: Object,     // content-script 抓的页面侧请求头（永不覆盖 networkHeaders）
 *     networkHeaders: Object,  // webRequest 抓的真实请求头（合并时优先）
 *     pageUrl: string,
 *     content: ?string,
 *     time: number
 *   }>
 * }
 */
const tabData = new Map();

/** 单调递增序号：同一毫秒内多条捕获也能稳定排序（time 只用于 TTL） */
let itemSeq = 0;

// ============================================================ 数据模型工具

/**
 * 取（必要时创建）某个 tab 的数据容器。
 * @param {number} tabId
 * @return {{title: string, items: !Map}}
 */
function ensureTab(tabId) {
  let tab = tabData.get(tabId);
  if (!tab) {
    tab = { title: "", items: new Map() };
    tabData.set(tabId, tab);
  }
  return tab;
}

/**
 * 创建一个新的捕获条目。
 * @param {string} url 已归一化的 URL
 * @param {string} kind "hls" / "dash"
 * @return {!Object}
 */
function createItem(url, kind) {
  return {
    url: url,
    kind: kind || "",
    pageHeaders: {},
    networkHeaders: {},
    pageUrl: "",
    content: null,
    time: 0,
    seq: 0,
  };
}

/**
 * 取或创建某个 tab 下的条目。
 * @param {number} tabId
 * @param {string} url 已归一化的 URL
 * @param {string} [kind] 已知格式；已有条目允许从空值补齐
 * @return {{tab: !Object, item: !Object, isNew: boolean}}
 */
function ensureItem(tabId, url, kind) {
  const tab = ensureTab(tabId);
  const existing = tab.items.get(url);
  if (existing) {
    if (kind && !existing.kind) existing.kind = kind;
    return { tab: tab, item: existing, isNew: false };
  }
  const item = createItem(url, kind);
  tab.items.set(url, item);
  return { tab: tab, item: item, isNew: true };
}

/**
 * 对单个 tab 执行 TTL 清理 + 数量上限（超限丢最旧的）。
 * @param {!Object} tab
 * @return {boolean} 是否有条目被删除
 */
function enforceLimits(tab) {
  if (!tab || !tab.items) return false;
  const now = Date.now();
  let changed = false;

  for (const [url, item] of tab.items) {
    if (now - (item && item.time ? item.time : 0) > ITEM_TTL) {
      tab.items.delete(url);
      changed = true;
    }
  }

  if (tab.items.size > MAX_ITEMS_PER_TAB) {
    // 按捕获序号淘汰最旧的（序号单调递增，同毫秒也稳定）
    const sorted = [...tab.items.entries()].sort((a, b) => (a[1] && a[1].seq ? a[1].seq : 0) - (b[1] && b[1].seq ? b[1].seq : 0));
    const overflow = tab.items.size - MAX_ITEMS_PER_TAB;
    for (let i = 0; i < overflow; i++) {
      tab.items.delete(sorted[i][0]);
      changed = true;
    }
  }
  return changed;
}

/**
 * 全量清理：过期条目 + 数量上限，删除空且无标题的 tab 数据。
 * @return {!Array<number>} 需要刷新角标的 tabId 列表
 */
function pruneAll() {
  const affected = [];
  for (const [tabId, tab] of tabData) {
    const before = tab.items.size;
    const changed = enforceLimits(tab);
    if (changed || tab.items.size !== before) affected.push(tabId);
    if (tab.items.size === 0 && !tab.title) tabData.delete(tabId);
  }
  return affected;
}

/**
 * 捕获之后的统一收尾：刷新角标 + 全局清理（并同步其它受影响 tab 的角标）。
 * @param {number} tabId
 */
function afterCapture(tabId) {
  // 先清理再刷角标，保证角标数字与列表条数一致
  const affected = pruneAll();
  updateBadge(tabId);
  for (const id of affected) {
    if (id !== tabId) updateBadge(id);
  }
}

/**
 * 把内部条目转换成 popup 需要的结构（headers 为合并结果，保持发送契约）。
 * @param {!Object} item
 * @return {!Object}
 */
function toPublicItem(item) {
  const kind = MC.normalizeKind(item.kind) || MC.streamKind(item.url);
  return {
    url: item.url,
    kind: kind,
    kindLabel: MC.kindLabel(kind),
    headers: MC.mergeHeaders(item.pageHeaders, item.networkHeaders),
    pageHeaders: item.pageHeaders,
    networkHeaders: item.networkHeaders,
    pageUrl: item.pageUrl || "",
    content: item.content || null,
    time: item.time || 0,
  };
}

/**
 * 取某个 tab 的列表（按时间倒序）。
 * @param {number} tabId
 * @param {string=} fallbackTitle 后台没有标题时用调用方（popup）提供的标题兜底
 * @return {{items: !Array<!Object>, title: string}}
 */
function listForTab(tabId, fallbackTitle) {
  const tab = tabData.get(tabId);
  if (!tab) return { items: [], title: typeof fallbackTitle === "string" ? fallbackTitle : "" };
  if (!tab.title && typeof fallbackTitle === "string" && fallbackTitle) tab.title = fallbackTitle.slice(0, MAX_TITLE_LENGTH);
  const items = [...tab.items.values()]
    .sort((a, b) => (b.seq || 0) - (a.seq || 0) || (b.time || 0) - (a.time || 0))
    .slice(0, MAX_ITEMS_PER_TAB)
    .map(toPublicItem);
  return { items: items, title: tab.title || "" };
}

/**
 * 更新某个 tab 的角标数量。
 * @param {number} tabId
 */
function updateBadge(tabId) {
  if (typeof tabId !== "number" || tabId < 0) return;
  const tab = tabData.get(tabId);
  const count = tab ? tab.items.size : 0;
  try {
    const textResult = chrome.action.setBadgeText({ tabId: tabId, text: count ? String(count) : "" });
    if (textResult && typeof textResult.catch === "function") textResult.catch(() => {});
    const colorResult = chrome.action.setBadgeBackgroundColor({ tabId: tabId, color: "#3f3f46" });
    if (colorResult && typeof colorResult.catch === "function") colorResult.catch(() => {});
  } catch (e) {
    /* tab 已关闭或角标不可用，忽略 */
  }
}

/**
 * 后台没有标题时，异步从 tabs API 补齐（不阻塞消息处理）。
 * @param {number} tabId
 */
function fillTitleFromTab(tabId) {
  const tab = tabData.get(tabId);
  if (!tab || tab.title) return;
  try {
    const pending = chrome.tabs.get(tabId);
    if (pending && typeof pending.then === "function") {
      pending
        .then((info) => {
          const current = tabData.get(tabId);
          if (current && !current.title && info && typeof info.title === "string" && info.title) {
            current.title = info.title.slice(0, MAX_TITLE_LENGTH);
          }
        })
        .catch(() => {});
    }
  } catch (e) {
    /* 忽略 */
  }
}

// ============================================================ 接收捕获

/**
 * 处理 content-script 上报的捕获：只更新 page 侧字段，绝不覆盖 networkHeaders。
 * @param {!Object} msg
 * @param {!Object} sender
 * @return {{ok: boolean, error: (string|undefined), count: (number|undefined)}}
 */
function handleCapture(msg, sender) {
  const tabId = sender && sender.tab && typeof sender.tab.id === "number" ? sender.tab.id : -1;
  if (tabId < 0) return { ok: false, error: "缺少 tab 上下文" };

  const url = MC.normalizeUrl(msg.url);
  if (!url) return { ok: false, error: "URL 不合法" };

  const content = typeof msg.content === "string" && msg.content ? msg.content.slice(0, MAX_CONTENT_LENGTH) : "";
  // 格式判定顺序：消息里显式带的 kind > URL 后缀 > 响应体内容（#EXTM3U / <MPD>）
  const kind = MC.normalizeKind(msg.kind) || MC.streamKind(url) || MC.contentKind(content);
  if (kind !== "hls" && kind !== "dash") {
    return { ok: false, error: "不是受支持的视频流（仅识别 HLS / DASH）" };
  }

  const senderTitle = sender.tab && typeof sender.tab.title === "string" ? sender.tab.title : "";
  const senderUrl = sender.tab && typeof sender.tab.url === "string" ? sender.tab.url : "";

  const { tab, item } = ensureItem(tabId, url, kind);
  if (senderTitle) tab.title = senderTitle.slice(0, MAX_TITLE_LENGTH);

  // 页面侧头：同键以新值为准（token 刷新后旧值必须被替换），但只落在 pageHeaders
  const incoming = MC.cleanHeaders(msg.headers);
  item.pageHeaders = Object.assign({}, item.pageHeaders, incoming);
  if (content) item.content = content;
  item.pageUrl = MC.normalizeUrl(msg.pageUrl) || item.pageUrl || MC.normalizeUrl(senderUrl) || "";
  item.time = Date.now();
  item.seq = ++itemSeq;
  if (!tab.title) fillTitleFromTab(tabId);

  afterCapture(tabId);
  return { ok: true, count: tab.items.size };
}

/**
 * 处理 webRequest 抓到的真实请求头。
 * @param {!Object} details
 */
function handleBeforeSendHeaders(details) {
  if (!details || typeof details.url !== "string") return;
  const tabId = typeof details.tabId === "number" ? details.tabId : -1;
  if (tabId < 0) return; // 无法归属到具体 tab 的请求无处展示，直接丢弃
  const url = MC.normalizeUrl(details.url);
  const kind = MC.streamKind(url);
  if (!url || !kind) return; // 只补 HLS / DASH 清单的真实请求头（分片不记录，避免刷屏）

  const headers = MC.cleanHeaders(details.requestHeaders);
  if (!Object.keys(headers).length && !details.initiator) return;

  const { item, isNew } = ensureItem(tabId, url, kind);
  // 网络头永远以最新一次真实请求为准，并覆盖同名的页面侧头
  item.networkHeaders = Object.assign({}, item.networkHeaders, headers);
  if (!item.pageUrl) item.pageUrl = MC.normalizeUrl(details.initiator) || "";
  item.time = Date.now();
  item.seq = ++itemSeq;
  if (isNew) fillTitleFromTab(tabId);

  afterCapture(tabId);
}

// ============================================================ 本地下载器通信

/**
 * 带超时的 fetch（AbortController + setTimeout，兼容 Chrome 102，不依赖 AbortSignal.timeout）。
 * @param {string} url
 * @param {!Object} options
 * @param {number} timeoutMs
 * @return {!Promise<!Response>}
 */
async function fetchWithTimeout(url, options, timeoutMs) {
  const controller = new AbortController();
  const timer = setTimeout(() => {
    try {
      controller.abort();
    } catch (e) {
      /* 忽略 */
    }
  }, timeoutMs);
  try {
    return await fetch(url, Object.assign({}, options || {}, { signal: controller.signal, cache: "no-store" }));
  } finally {
    clearTimeout(timer);
  }
}

/**
 * 探测本地下载器是否在线，并校验响应里的 app 标识（防止端口被其它服务占用时误判已连接）。
 * @return {!Promise<{online: boolean, error: (string|undefined), version: (string|undefined)}>}
 */
async function pingApp() {
  try {
    const resp = await fetchWithTimeout(`${APP_URL}/ping`, { method: "GET" }, PING_TIMEOUT_MS);
    if (!resp.ok) return { online: false, error: `HTTP ${resp.status}` };
    let data = null;
    try {
      data = await resp.json();
    } catch (e) {
      return { online: false, error: "响应不是 JSON" };
    }
    if (!data || data.app !== "HLSink") return { online: false, error: "端口被其它服务占用" };
    return { online: true, version: typeof data.version === "string" ? data.version : "" };
  } catch (e) {
    const aborted = e && e.name === "AbortError";
    return { online: false, error: aborted ? "连接下载器超时" : String((e && e.message) || e) };
  }
}

/**
 * 发送单个任务到本地下载器，严格校验 HTTP 状态、JSON 与 data.id。
 * @param {!Object} rawTask
 * @return {!Promise<{ok: boolean, id: (string|undefined), error: (string|undefined)}>}
 */
async function sendTask(rawTask) {
  if (!rawTask || typeof rawTask !== "object") return { ok: false, error: "任务为空" };

  const url = MC.normalizeUrl(rawTask.url);
  if (!url) return { ok: false, error: "URL 不合法（仅支持 http/https 绝对地址）" };

  const task = { url: url };
  // 格式由扩展判定：hls / dash 走 N_m3u8DL-RE，page（网页地址）交给 yt-dlp 解析
  const kind = MC.normalizeKind(rawTask.kind) || MC.streamKind(url) || "page";
  task.kind = kind;
  const headers = MC.filterSendHeaders(rawTask.headers);
  if (Object.keys(headers).length) task.headers = headers;

  const referer = MC.normalizeUrl(rawTask.referer);
  if (referer) task.referer = referer;

  const title = typeof rawTask.title === "string" ? rawTask.title.trim().slice(0, 120) : "";
  if (title) task.title = title;

  try {
    const resp = await fetchWithTimeout(
      `${APP_URL}/task`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(task),
      },
      SEND_TIMEOUT_MS
    );
    if (!resp.ok) {
      // 下载器 4xx 会在 body 里给出原因（校验失败 / 队列已满 / Origin 被拒），尽量透出
      let detail = "";
      try {
        const text = await resp.text();
        detail = typeof text === "string" ? text.trim().replace(/\s+/g, " ").slice(0, 160) : "";
      } catch (e) {
        detail = "";
      }
      return { ok: false, error: describeHttpError(resp.status, detail) };
    }

    let data = null;
    try {
      data = await resp.json();
    } catch (e) {
      return { ok: false, error: "下载器响应不是 JSON" };
    }
    const rawId = data && typeof data === "object" ? data.id : null;
    const id = typeof rawId === "string" || typeof rawId === "number" ? String(rawId) : "";
    if (!id) {
      const message = data && typeof data === "object" ? data.error || data.message : "";
      return { ok: false, error: typeof message === "string" && message ? message : "下载器未返回任务 id" };
    }
    // duplicate=true 表示同 URL 的任务已在队列中，仍然算发送成功
    return { ok: true, id: id, duplicate: !!(data && data.duplicate === true) };
  } catch (e) {
    const aborted = e && e.name === "AbortError";
    return { ok: false, error: aborted ? "连接下载器超时" : `无法连接下载器：${String((e && e.message) || e)}` };
  }
}

/**
 * 把下载器的 4xx/5xx 状态码翻译成可读错误。
 * @param {number} status
 * @param {string} detail 响应正文摘要
 * @return {string}
 */
function describeHttpError(status, detail) {
  const suffix = detail ? `：${detail}` : "";
  switch (status) {
    case 400:
      return `任务校验失败（HTTP 400）${suffix}`;
    case 403:
      return `下载器拒绝了扩展请求（HTTP 403，Origin 校验失败）${suffix}`;
    case 404:
      return `下载器接口不存在（HTTP 404）${suffix}`;
    case 413:
      return `任务数据过大（HTTP 413）${suffix}`;
    case 429:
      return `下载器队列已满，请稍后重试（HTTP 429）${suffix}`;
    default:
      return `下载器返回 HTTP ${status}${suffix}`;
  }
}

// ============================================================ 消息路由

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (!msg || typeof msg !== "object" || typeof msg.type !== "string") return undefined;

  switch (msg.type) {
    case "m3u8-found":
    case "stream-found": {
      sendResponse(handleCapture(msg, sender || {}));
      return undefined;
    }
    case "mc-get-list": {
      const tabId = typeof msg.tabId === "number" ? msg.tabId : -1;
      if (tabId < 0) {
        sendResponse({ items: [], title: "", error: "缺少 tabId" });
        return undefined;
      }
      sendResponse(listForTab(tabId, msg.tabTitle));
      return undefined;
    }
    case "mc-clear": {
      const tabId = typeof msg.tabId === "number" ? msg.tabId : -1;
      if (tabId >= 0) {
        tabData.delete(tabId);
        updateBadge(tabId);
      }
      sendResponse({ ok: true });
      return undefined;
    }
    case "mc-ping": {
      pingApp().then(sendResponse, (e) => sendResponse({ online: false, error: String((e && e.message) || e) }));
      return true;
    }
    case "mc-send-task": {
      sendTask(msg.task).then(sendResponse, (e) =>
        sendResponse({ ok: false, error: String((e && e.message) || e) })
      );
      return true;
    }
    default:
      return undefined;
  }
});

// ============================================================ webRequest 抓真实请求头

/**
 * 注册 webRequest 头监听：优先带 extraHeaders（可拿到 Cookie 等敏感头），
 * 不支持时降级为仅 requestHeaders。
 */
function registerHeaderListener() {
  const filter = { urls: ["<all_urls>"] };
  try {
    chrome.webRequest.onBeforeSendHeaders.addListener(handleBeforeSendHeaders, filter, [
      "requestHeaders",
      "extraHeaders",
    ]);
    return true;
  } catch (e) {
    try {
      chrome.webRequest.onBeforeSendHeaders.addListener(handleBeforeSendHeaders, filter, ["requestHeaders"]);
    } catch (e2) {
      return false;
    }
    return false;
  }
}

registerHeaderListener();

// ============================================================ 生命周期 / 清理

/**
 * 周期清理：过期条目 + 数量上限，并同步刷新受影响 tab 的角标。
 */
function pruneAndSync() {
  const affected = pruneAll();
  for (const tabId of affected) updateBadge(tabId);
}

function ensureAlarm() {
  try {
    const pending = chrome.alarms.create(PRUNE_ALARM, { periodInMinutes: PRUNE_PERIOD_MINUTES });
    if (pending && typeof pending.catch === "function") pending.catch(() => {});
  } catch (e) {
    /* 忽略：alarm 不可用时仍会在每次捕获后清理 */
  }
}

chrome.alarms.onAlarm.addListener((alarm) => {
  if (!alarm || alarm.name !== PRUNE_ALARM) return;
  pruneAndSync();
});

chrome.tabs.onRemoved.addListener((tabId) => {
  if (!tabData.has(tabId)) return;
  tabData.delete(tabId);
  try {
    const pending = chrome.action.setBadgeText({ tabId: tabId, text: "" });
    if (pending && typeof pending.catch === "function") pending.catch(() => {});
  } catch (e) {
    /* tab 已关闭，忽略 */
  }
});

chrome.tabs.onUpdated.addListener((tabId, changeInfo, tab) => {
  if (changeInfo && changeInfo.status === "loading") {
    // 新页面开始加载：清空旧页捕获与旧页标题，避免角标/标题残留
    const data = tabData.get(tabId);
    if (data) {
      data.items.clear();
      data.title = "";
    }
    updateBadge(tabId);
    return;
  }
  const nextTitle = (changeInfo && typeof changeInfo.title === "string" ? changeInfo.title : "") ||
    (tab && typeof tab.title === "string" ? tab.title : "");
  if (!nextTitle) return;
  const data = tabData.get(tabId);
  if (!data) return; // 没有捕获数据的 tab 不建缓存
  data.title = nextTitle.slice(0, MAX_TITLE_LENGTH);
});

// ============================================================ 注入脚本注册

/**
 * 同步注入脚本：bridge（ISOLATED world，负责转发）+ inject（MAIN world，负责拦截）。
 * @param {boolean} force true 时先反注册，用于扩展更新后应用新配置
 * @return {!Promise<void>}
 */
async function syncContentScripts(force) {
  const wanted = [
    {
      id: "miaocatch-bridge",
      matches: ["http://*/*", "https://*/*"],
      js: ["js/bridge.js"],
      runAt: "document_start",
      world: "ISOLATED",
      allFrames: true,
      persistAcrossSessions: true,
    },
    {
      id: "miaocatch-inject",
      matches: ["http://*/*", "https://*/*"],
      js: ["js/common.js", "js/inject.js"],
      runAt: "document_start",
      world: "MAIN",
      allFrames: true,
      persistAcrossSessions: true,
    },
  ];

  try {
    if (force) {
      try {
        await chrome.scripting.unregisterContentScripts({ ids: SCRIPT_IDS });
      } catch (e) {
        /* 未注册过，忽略 */
      }
    }
    const existing = await chrome.scripting.getRegisteredContentScripts();
    const registeredIds = new Set((existing || []).map((script) => script.id));
    const missing = wanted.filter((script) => !registeredIds.has(script.id));
    if (missing.length) await chrome.scripting.registerContentScripts(missing);
  } catch (e) {
    /* 注入脚本注册失败不应影响其它功能，真实 Chrome 中会记录到扩展错误页 */
  }
}

chrome.runtime.onInstalled.addListener(() => {
  ensureAlarm();
  syncContentScripts(true);
  pruneAndSync();
});

chrome.runtime.onStartup.addListener(() => {
  ensureAlarm();
  pruneAndSync();
});

// Service Worker 每次唤醒都会执行到这里：确保 alarm 存在、注入脚本已注册
ensureAlarm();
syncContentScripts(false);
