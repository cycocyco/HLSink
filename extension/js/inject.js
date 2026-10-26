/**
 * HLSink注入脚本（MAIN world, document_start, allFrames）。
 *
 * 运行在页面环境里，拦截 fetch / XMLHttpRequest，抓取带 token 的 HLS（m3u8）/
 * DASH（mpd）地址与页面侧请求头。
 * 抓到的结果通过两种途径上报：
 *   1) 能拿到扩展 runtime（chrome.runtime.id 存在）时直接 chrome.runtime.sendMessage；
 *   2) 否则 window.postMessage 交给隔离环境的 js/bridge.js 转发。
 *
 * 依赖 js/common.js 提供的 MiaocatchCommon（同批次注入，执行顺序在前）。
 */
(function () {
  "use strict";

  if (window.__miaocatch_injected__) return;
  window.__miaocatch_injected__ = true;

  var MC = window.MiaocatchCommon;
  if (!MC) return; // common.js 未就绪，放弃注入（不抛错污染页面）

  var BRIDGE_KEY = "__miaocatch__";
  var MAX_URL_LENGTH = MC.MAX_URL_LENGTH;
  var MAX_CONTENT_LENGTH = MC.MAX_CONTENT_LENGTH;
  /** 同一 URL 的最小重报间隔：token / header 刷新后允许再次上报 */
  var RECAPTURE_INTERVAL = 3000;
  /** seen 表容量上限（按最后上报时间淘汰） */
  var SEEN_LIMIT = 200;
  /** 响应体过大时跳过文本嗅探（避免把大文件读进内存） */
  var MAX_SNIFF_BYTES = 2 * 1024 * 1024;

  /** url -> 最后上报时间戳，允许时间窗过后再次更新（不阻断 header 刷新） */
  var seen = new Map();

  /**
   * 当前环境是否真的能调用扩展 runtime（MAIN world 里 chrome.runtime 可能只是网页版对象）。
   * @return {boolean}
   */
  function hasExtRuntime() {
    try {
      return !!(
        typeof chrome !== "undefined" &&
        chrome &&
        chrome.runtime &&
        typeof chrome.runtime.sendMessage === "function" &&
        typeof chrome.runtime.id === "string" &&
        chrome.runtime.id
      );
    } catch (e) {
      return false;
    }
  }

  var useExtRuntime = hasExtRuntime();

  /**
   * 上报捕获结果给 background（两条路径二选一，避免重复消息）。
   * @param {!Object} message
   */
  function report(message) {
    if (useExtRuntime) {
      try {
        var pending = chrome.runtime.sendMessage(message);
        if (pending && typeof pending.catch === "function") pending.catch(function () {});
        return;
      } catch (e) {
        useExtRuntime = false; // 运行时失效（扩展重载等），降级走 postMessage
      }
    }
    try {
      var payload = {};
      payload[BRIDGE_KEY] = "capture";
      payload.payload = message;
      window.postMessage(payload, "*");
    } catch (e) {
      /* 忽略：上报失败不影响页面 */
    }
  }

  /**
   * 判断本次响应体是否值得读文本做流清单嗅探（避免把大文件读进内存）。
   * @param {string} contentType
   * @param {number} contentLength
   * @param {string} url 已归一化的最终 URL
   * @return {boolean}
   */
  function shouldSniffContent(contentType, contentLength, url) {
    if (contentLength > MAX_SNIFF_BYTES) return false;
    var ct = String(contentType || "").toLowerCase();
    if (ct.indexOf("mpegurl") >= 0 || ct.indexOf("m3u") >= 0) return true;
    if (ct.indexOf("dash") >= 0 || ct.indexOf("xml") >= 0) return true;
    if (ct.indexOf("text/plain") >= 0 || ct.indexOf("octet-stream") >= 0) return true;
    if (ct) return false; // 明确的其它类型（json / 图片 / 视频…）不嗅探
    // 完全没有 content-type 时，只在 URL 无文件扩展名的情况下嗅探，避免读整段视频/图片
    var pathname = "";
    try {
      pathname = new URL(url).pathname || "";
    } catch (e) {
      pathname = String(url || "");
    }
    return !/\.[a-z0-9]{1,5}$/i.test(pathname);
  }

  /**
   * 记录一次捕获。
   * @param {*} rawUrl 原始 URL（相对地址会按当前页面补全）
   * @param {!Object} headers 页面侧请求头
   * @param {{content: (string|undefined)}=} extra 可选的清单文本
   */
  function capture(rawUrl, headers, extra) {
    var url = MC.normalizeUrl(rawUrl, window.location.href);
    if (!url) return;

    var content = extra && typeof extra.content === "string" && extra.content ? extra.content : "";
    // URL 后缀优先；没有后缀时再看响应体是 #EXTM3U 还是 <MPD>
    var kind = MC.streamKind(url) || MC.contentKind(content);
    if (!kind) return;

    var now = Date.now();
    var last = seen.get(url) || 0;
    if (now - last < RECAPTURE_INTERVAL) return; // 时间窗内去抖，窗口外允许更新
    seen.delete(url);
    seen.set(url, now);
    if (seen.size > SEEN_LIMIT) {
      var entries = [];
      seen.forEach(function (time, key) {
        entries.push([key, time]);
      });
      entries.sort(function (a, b) {
        return a[1] - b[1];
      });
      var drop = entries.length - SEEN_LIMIT;
      for (var i = 0; i < drop; i++) seen.delete(entries[i][0]);
    }

    var message = {
      // 沿用旧消息名以兼容尚未刷新的页面脚本，kind 字段区分 HLS / DASH
      type: "m3u8-found",
      kind: kind,
      url: url,
      headers: MC.cleanHeaders(headers),
      pageUrl: MC.normalizeUrl(window.location.href) || ""
    };
    if (content) message.content = content.slice(0, MAX_CONTENT_LENGTH);
    report(message);
  }

  // ---------------------------------------------------------------- fetch 拦截
  var origFetch = window.fetch;
  if (typeof origFetch === "function") {
    window.fetch = function mcFetch(input, init) {
      var rawUrl = "";
      var headers = {};
      try {
        if (typeof input === "string") {
          rawUrl = input;
        } else if (input && typeof input === "object" && typeof input.url === "string") {
          rawUrl = input.url; // Request 对象
        } else if (input && typeof input.href === "string") {
          rawUrl = input.href; // URL 对象
        } else {
          rawUrl = String(input);
        }
        if (input && typeof input === "object" && input.headers) headers = MC.cleanHeaders(input.headers);
        if (init && init.headers) {
          var initHeaders = MC.cleanHeaders(init.headers);
          var keys = Object.keys(initHeaders);
          for (var i = 0; i < keys.length; i++) headers[keys[i]] = initHeaders[keys[i]];
        }
      } catch (e) {
        headers = {};
      }

      var requestUrl = rawUrl.length <= MAX_URL_LENGTH ? MC.normalizeUrl(rawUrl, window.location.href) : "";
      var promise = origFetch.apply(this, arguments);
      if (!requestUrl || !promise || typeof promise.then !== "function") return promise;

      return promise.then(function (resp) {
        try {
          inspectFetchResponse(resp, requestUrl, headers);
        } catch (e) {
          /* 嗅探失败不能影响页面拿到的响应 */
        }
        return resp;
      });
    };
  }

  /**
   * 检查 fetch 响应是否是 m3u8，是则上报（带响应正文）。
   * @param {!Response} resp
   * @param {string} requestUrl 已归一化的请求 URL
   * @param {!Object} headers 页面侧请求头
   */
  function inspectFetchResponse(resp, requestUrl, headers) {
    if (!resp || typeof resp !== "object") return;
    // 重定向后 resp.url 才是真实地址
    var finalUrl = MC.normalizeUrl(resp.url, window.location.href) || requestUrl;
    var contentType = "";
    var contentLength = 0;
    try {
      contentType = (resp.headers && resp.headers.get("content-type")) || "";
      contentLength = parseInt((resp.headers && resp.headers.get("content-length")) || "0", 10) || 0;
    } catch (e) {
      contentType = "";
    }

    var bySuffix = MC.isStreamUrl(finalUrl) || MC.isStreamUrl(requestUrl);
    if (bySuffix) capture(finalUrl, headers, null);
    // content-type 可能是 text/plain / octet-stream / xml（很多 CDN 这样发清单），统一交给内容嗅探兜底
    if (!shouldSniffContent(contentType, contentLength, finalUrl)) return;

    var clone = null;
    try {
      clone = resp.clone();
    } catch (e) {
      return; // body 已被消费，无法嗅探
    }
    clone
      .text()
      .then(function (text) {
        if (MC.contentKind(text)) {
          capture(finalUrl, headers, { content: text });
        }
      })
      .catch(function () {});
  }

  // ---------------------------------------------------------------- XHR 拦截
  var XHRProto = window.XMLHttpRequest && window.XMLHttpRequest.prototype;
  if (XHRProto && typeof XHRProto.open === "function") {
    var origOpen = XHRProto.open;
    var origSetHeader = XHRProto.setRequestHeader;
    var origSend = XHRProto.send;

    XHRProto.open = function mcOpen(method, url) {
      try {
        // XHR.open 常传相对地址，必须按当前页面归一化
        var raw = typeof url === "string" ? url : url && typeof url.href === "string" ? url.href : String(url);
        this.__mc_url = raw.length <= MAX_URL_LENGTH ? MC.normalizeUrl(raw, window.location.href) : "";
        this.__mc_headers = {};
      } catch (e) {
        this.__mc_url = "";
        this.__mc_headers = {};
      }
      return origOpen.apply(this, arguments);
    };

    XHRProto.setRequestHeader = function mcSetRequestHeader(name, value) {
      try {
        if (this.__mc_headers && typeof name === "string") this.__mc_headers[name] = value;
      } catch (e) {
        /* 忽略 */
      }
      return origSetHeader.apply(this, arguments);
    };

    XHRProto.send = function mcSend() {
      try {
        watchXhr(this);
      } catch (e) {
        /* 忽略 */
      }
      return origSend.apply(this, arguments);
    };
  }

  /**
   * 给一个 XHR 实例挂 load 监听，加载完成后判断是否是 m3u8 并上报。
   * @param {!XMLHttpRequest} xhr
   */
  function watchXhr(xhr) {
    if (!xhr || !xhr.__mc_url) return;
    var requestUrl = xhr.__mc_url;
    xhr.addEventListener("load", function () {
      try {
        var finalUrl = MC.normalizeUrl(xhr.responseURL, window.location.href) || requestUrl;
        var contentType = "";
        try {
          contentType = xhr.getResponseHeader("content-type") || "";
        } catch (e) {
          contentType = "";
        }
        var bySuffix = MC.isStreamUrl(finalUrl) || MC.isStreamUrl(requestUrl);
        if (bySuffix) capture(finalUrl, xhr.__mc_headers, null);
        if (!shouldSniffContent(contentType, 0, finalUrl)) return;

        var responseType = xhr.responseType;
        if (responseType !== "" && responseType !== "text") return; // 二进制响应不读
        var text = "";
        try {
          text = xhr.responseText;
        } catch (e) {
          return;
        }
        if (MC.contentKind(text)) {
          capture(finalUrl, xhr.__mc_headers, { content: text });
        }
      } catch (e) {
        /* 嗅探失败不影响页面 */
      }
    });
  }
})();
