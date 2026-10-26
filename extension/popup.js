/**
 * HLSink popup：列出当前 tab 的嗅探结果，勾选后发送到本地下载器。
 *
 * 依赖 js/common.js（提供 MiaocatchCommon：headers 过滤 / URL 归一化），
 * 依赖 background 的 mc-ping / mc-get-list / mc-clear / mc-send-task 消息。
 */
(function () {
  "use strict";

  const MC = globalThis.MiaocatchCommon;
  /** 桌面端注册的本地协议；离线时用 Windows 拉起HLSink。 */
  const APP_PROTOCOL = "hlsink://open";

  /** @type {!Array<!Object>} 当前列表数据 */
  let items = [];
  /** @type {(number|null)} 当前 tabId */
  let tabId = null;
  /** @type {string} 页面标题，用于 autoName */
  let pageTitle = "";
  /** @type {string} 当前页面地址：没有嗅探到流时可整页交给 yt-dlp 解析 */
  let pageUrl = "";
  /** @type {boolean} 防止双击重复发送 */
  let sending = false;

  document.addEventListener("DOMContentLoaded", () => {
    init();
  });

  /** 初始化：绑定按钮 + 探测下载器 + 拉取列表 */
  async function init() {
    bindButtons();
    try {
      const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
      const tab = tabs && tabs[0];
      if (!tab || typeof tab.id !== "number") {
        tip("无法获取当前标签页", true);
        setSendEnabled(false);
        renderMessage("无法获取当前标签页");
        return;
      }
      tabId = tab.id;
      pageTitle = typeof tab.title === "string" ? tab.title : "";
      pageUrl = typeof tab.url === "string" ? MC.normalizeUrl(tab.url) : "";
    } catch (e) {
      tip("读取标签页失败：" + errText(e), true);
      setSendEnabled(false);
      renderMessage("读取标签页失败");
      return;
    }

    const online = await checkPing();
    if (!online) setSendEnabled(false);
    await load();
  }

  /** 绑定刷新 / 清空 / 发送按钮 */
  function bindButtons() {
    const refreshBtn = document.getElementById("refresh");
    if (refreshBtn) refreshBtn.onclick = () => load();

    const clearBtn = document.getElementById("clear");
    if (clearBtn) {
      clearBtn.onclick = async () => {
        try {
          await chrome.runtime.sendMessage({ type: "mc-clear", tabId: tabId });
        } catch (e) {
          tip("清空失败：" + errText(e), true);
        }
        await load();
      };
    }

    const sendBtn = document.getElementById("send");
    if (sendBtn) sendBtn.onclick = () => send();

    const launchBtn = document.getElementById("launch-app");
    if (launchBtn) launchBtn.onclick = () => launchApp();
  }

  /**
   * 探测本地下载器是否在线（走 background，避免 popup 直接受 CORS 影响）。
   * @return {!Promise<boolean>}
   */
  async function checkPing() {
    let resp = null;
    try {
      resp = await chrome.runtime.sendMessage({ type: "mc-ping" });
    } catch (e) {
      resp = null;
    }
    if (resp && resp.online === true) {
      setLaunchVisible(false);
      setSendEnabled(true);
      tip("已连接HLSink下载器");
      return true;
    }
    const reason = resp && typeof resp.error === "string" && resp.error ? resp.error : "";
    setLaunchVisible(true);
    setSendEnabled(false);
    tip("未检测到HLSink下载器" + (reason ? `（${reason}）` : "") + "，可点击下方启动", true);
    return false;
  }

  /**
   * 显示/隐藏离线启动按钮。
   * @param {boolean} visible
   */
  function setLaunchVisible(visible) {
    const btn = document.getElementById("launch-app");
    if (!btn) return;
    btn.classList.toggle("hide", !visible);
    btn.disabled = false;
    btn.textContent = "启动 HLSink";
  }

  /** 等待指定时间。 */
  function sleep(ms) {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }

  /**
   * 用本地协议拉起HLSink，并在弹窗仍存在时等待它启动。
   * 浏览器首次会询问是否允许打开外部应用，这是系统安全提示。
   */
  async function launchApp() {
    const btn = /** @type {?HTMLButtonElement} */ (document.getElementById("launch-app"));
    if (!btn || btn.disabled) return;
    btn.disabled = true;
    btn.textContent = "正在启动…";
    tip("正在通过本地协议启动 HLSink");

    try {
      const link = document.createElement("a");
      link.href = APP_PROTOCOL;
      link.style.display = "none";
      document.body.appendChild(link);
      link.click();
      link.remove();
    } catch (e) {
      btn.disabled = false;
      btn.textContent = "启动 HLSink";
      tip("无法调用本地协议：" + errText(e), true);
      return;
    }

    // 若浏览器没有因外部协议关闭弹窗，就继续等待 /ping 恢复。
    for (let i = 0; i < 10; i++) {
      await sleep(700);
      if (document.hidden) return;
      let resp = null;
      try {
        resp = await chrome.runtime.sendMessage({ type: "mc-ping" });
      } catch (e) {
        resp = null;
      }
      if (resp && resp.online === true) {
        setLaunchVisible(false);
        setSendEnabled(true);
        tip("HLSink已启动");
        return;
      }
    }

    btn.disabled = false;
    btn.textContent = "再次启动 HLSink";
    tip("仍未检测到HLSink；请确认已运行过一次新版 APP，并允许浏览器打开本地应用", true);
  }

  /** 拉取当前 tab 的嗅探结果 */
  async function load() {
    if (tabId === null) return;
    let resp = null;
    try {
      resp = await chrome.runtime.sendMessage({ type: "mc-get-list", tabId: tabId, tabTitle: pageTitle });
    } catch (e) {
      items = [];
      renderMessage("读取嗅探结果失败：" + errText(e));
      return;
    }
    // background 可能正在重启，响应可能为 undefined
    if (!resp || typeof resp !== "object") {
      items = [];
      renderMessage("后台未响应，请关闭弹窗后重试");
      return;
    }
    items = Array.isArray(resp.items) ? resp.items : [];
    if (typeof resp.title === "string" && resp.title) pageTitle = resp.title;
    render();
  }

  /** 渲染列表 */
  function render() {
    const list = document.getElementById("list");
    if (!list) return;
    list.innerHTML = "";

    if (!items.length) {
      renderMessage("");
      return;
    }

    for (const item of items) {
      const row = document.createElement("label");
      row.className = "item is-selected";

      const box = document.createElement("input");
      box.type = "checkbox";
      box.checked = true;
      box.dataset.url = item.url;

      const info = document.createElement("div");
      info.className = "info";

      const nameEl = document.createElement("div");
      nameEl.className = "name";
      nameEl.textContent = guessName(item.url);
      nameEl.title = nameEl.textContent;

      const urlEl = document.createElement("div");
      urlEl.className = "url";
      urlEl.textContent = item.url;
      urlEl.title = item.url;

      const tagsEl = document.createElement("div");
      tagsEl.className = "tags";
      const kind = MC.normalizeKind(item.kind) || MC.streamKind(item.url);
      if (kind) {
        const kindTag = tag(MC.kindLabel(kind) || kind);
        kindTag.className = "tag is-format";
        tagsEl.appendChild(kindTag);
      }
      if (item.content) tagsEl.appendChild(tag("已缓存"));
      const headerCount = item.headers ? Object.keys(item.headers).length : 0;
      if (headerCount) tagsEl.appendChild(tag(headerCount + " 个请求头"));
      if (!tagsEl.children.length) tagsEl.style.display = "none";

      box.addEventListener("change", () => {
        row.classList.toggle("is-selected", box.checked);
        updateSelectionSummary();
      });

      info.appendChild(nameEl);
      info.appendChild(urlEl);
      info.appendChild(tagsEl);
      row.appendChild(box);
      row.appendChild(info);
      list.appendChild(row);
    }
    updateSelectionSummary();
  }

  /** 更新列表顶部数量与已选数量。 */
  function updateSelectionSummary() {
    const summary = document.getElementById("summary");
    const selection = document.getElementById("selection-summary");
    if (summary) summary.textContent = items.length ? `本页 ${items.length} 条` : "本页嗅探结果";
    if (!selection) return;
    if (!items.length) {
      selection.textContent = "勾选后发送";
      return;
    }
    const checked = document.querySelectorAll(".item input[type=checkbox]:checked").length;
    selection.textContent = checked ? `已选 ${checked} / ${items.length}` : "未选择任务";
  }

  /**
   * 生成一个小标签元素。
   * @param {string} text
   * @return {!HTMLElement}
   */
  function tag(text) {
    const span = document.createElement("span");
    span.className = "tag";
    span.textContent = text;
    return span;
  }

  /**
   * 列表区域显示一段提示文本。
   * @param {string} text
   */
  function renderMessage(text) {
    const list = document.getElementById("list");
    if (!list) return;
    list.innerHTML = "";
    const box = document.createElement("div");
    box.className = "empty";
    const mark = document.createElement("div");
    mark.className = "empty-mark";
    mark.setAttribute("aria-hidden", "true");
    const title = document.createElement("div");
    title.className = "empty-title";
    title.textContent = text || "本页未发现视频流";
    const sub = document.createElement("div");
    sub.className = "empty-sub";
    sub.textContent = text ? "请刷新页面或重新播放视频" : "播放一下视频再刷新试试";
    box.appendChild(mark);
    box.appendChild(title);
    box.appendChild(sub);
    // 嗅探不到流时，允许把整页地址交给下载器解析（交给 yt-dlp）
    if (!text && pageUrl) {
      const pageBtn = document.createElement("button");
      pageBtn.type = "button";
      pageBtn.className = "page-send";
      pageBtn.textContent = "用本页地址下载";
      pageBtn.title = "把当前页面地址发给下载器，由其解析站内视频";
      pageBtn.onclick = () => sendPage();
      box.appendChild(pageBtn);
    }
    list.appendChild(box);
    updateSelectionSummary();
  }

  /**
   * 用当前页面地址创建一个任务（kind=page，后端交给 yt-dlp 解析）。
   */
  async function sendPage() {
    if (sending) return;
    const url = MC.normalizeUrl(pageUrl);
    if (!url) {
      tip("当前页面地址不支持下载", true);
      return;
    }
    sending = true;
    setSendBusy(true);
    try {
      const resp = await chrome.runtime.sendMessage({
        type: "mc-send-task",
        task: { url: url, title: autoName(url), kind: "page" },
      });
      if (resp && resp.ok) {
        tip(resp.duplicate ? "该页面已在下载中 ✓" : "已按网页地址发送 ✓");
        setTimeout(() => window.close(), 800);
        return;
      }
      tip("发送失败：" + (resp && resp.error ? resp.error : "后台未响应"), true);
    } catch (e) {
      tip("发送失败：" + errText(e), true);
    } finally {
      sending = false;
      setSendBusy(false);
    }
  }

  /**
   * 自动命名兜底：页面标题 > URL 段名。
   * @param {string} url
   * @return {string}
   */
  function autoName(url) {
    if (pageTitle) {
      // 视频站标题多为「名字 - 站名」，砍掉站名后缀
      const cleaned = pageTitle.split(/ [-_|] /)[0].trim();
      if (cleaned && cleaned.length > 3) return cleaned.slice(0, 80);
    }
    return guessName(url);
  }

  /**
   * 从 URL 里猜文件名。
   * @param {string} url
   * @return {string}
   */
  function guessName(url) {
    try {
      const parsed = new URL(url);
      const segment = parsed.pathname.split("/").filter(Boolean).pop() || parsed.hostname;
      let decoded = segment;
      try {
        decoded = decodeURIComponent(segment);
      } catch (e) {
        decoded = segment;
      }
      const withoutExt = decoded.replace(/\.(m3u8|m3u|mpd)$/i, "");
      return withoutExt || parsed.hostname;
    } catch (e) {
      return String(url || "").slice(0, 40);
    }
  }

  /**
   * 构造待发送任务列表。手填文件名只在单选时生效，多选各用自动命名并去重。
   * @param {!Array<string>} urls 勾选的 URL
   * @param {string} manualName 手填文件名
   * @return {!Array<!Object>}
   */
  function buildTasks(urls, manualName) {
    const single = urls.length === 1;
    const usedNames = new Map();
    const tasks = [];

    for (const url of urls) {
      const item = items.find((entry) => entry && entry.url === url);
      const headers = MC.filterSendHeaders(item ? item.headers : {});

      let title = "";
      if (single && manualName) {
        title = manualName;
      } else {
        const base = autoName(url).trim() || guessName(url);
        const count = (usedNames.get(base) || 0) + 1;
        usedNames.set(base, count);
        title = count > 1 ? `${base} (${count})` : base;
      }

      const task = { url: url, headers: headers, title: title };
      const referer = item && typeof item.pageUrl === "string" ? MC.normalizeUrl(item.pageUrl) : "";
      if (referer) task.referer = referer;
      tasks.push(task);
    }
    return tasks;
  }

  /** 发送勾选的任务（单条失败不中断整批） */
  async function send() {
    if (sending) return;

    const urls = [...document.querySelectorAll(".item input[type=checkbox]:checked")]
      .map((node) => node.dataset.url)
      .filter((url) => typeof url === "string" && url);
    if (!urls.length) {
      tip("请先勾选要下载的视频", true);
      return;
    }

    const manualNameEl = document.getElementById("filename");
    const manualName = manualNameEl ? manualNameEl.value.trim() : "";
    const tasks = buildTasks(urls, manualName);

    sending = true;
    setSendBusy(true);

    let sent = 0;
    let duplicates = 0;
    const errors = [];
    for (const task of tasks) {
      try {
        const resp = await chrome.runtime.sendMessage({ type: "mc-send-task", task: task });
        if (resp && resp.ok) {
          sent++;
          if (resp.duplicate) duplicates++; // 同 URL 任务已在下载器中
        } else {
          errors.push(resp && resp.error ? resp.error : "后台未响应");
        }
      } catch (e) {
        errors.push(errText(e));
      }
    }

    sending = false;
    setSendBusy(false);

    if (!errors.length) {
      tip(`已发送 ${sent} 个任务${duplicates ? `（其中 ${duplicates} 个已在下载中）` : ""} ✓`);
      setTimeout(() => window.close(), 800);
      return;
    }
    // 完成整批后再报告结果，失败时保留弹窗让用户看到原因
    tip(`成功 ${sent} 个，失败 ${errors.length} 个：${errors[0]}`, true);
    await load();
  }

  /**
   * 发送中禁用按钮，防止双击重复提交。
   * @param {boolean} busy
   */
  function setSendBusy(busy) {
    const btn = document.getElementById("send");
    if (!btn) return;
    btn.disabled = busy;
    btn.textContent = busy ? "发送中…" : "发送任务";
  }

  /**
   * 设置发送按钮可用状态。
   * @param {boolean} enabled
   */
  function setSendEnabled(enabled) {
    const btn = document.getElementById("send");
    if (btn) btn.disabled = !enabled;
  }

  /**
   * 提取错误文本。
   * @param {*} error
   * @return {string}
   */
  function errText(error) {
    if (!error) return "未知错误";
    if (typeof error === "string") return error;
    return String(error.message || error);
  }

  /**
   * 底部状态提示。
   * @param {string} text
   * @param {boolean=} isError
   */
  function tip(text, isError) {
    const el = document.getElementById("tip");
    if (!el) return;
    el.textContent = text;
    el.title = text || "";
    el.className = isError ? "offline" : "";
  }
})();
