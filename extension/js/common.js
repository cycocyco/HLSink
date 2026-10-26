/**
 * HLSink公共纯函数：URL 归一化、流格式判定（HLS / DASH）、请求头清洗与合并。
 *
 * 使用方：
 *   - js/background.js 通过 importScripts("common.js") 引入（Service Worker 全局作用域）
 *   - js/inject.js     通过 registerContentScripts 的 js 列表一起注入（MAIN world）
 *   - popup.html       通过 <script src="js/common.js"> 引入
 *
 * 本文件不依赖任何 chrome.* / DOM API，可直接在 Node 中加载做单元测试：
 *   require("./js/common.js"); globalThis.MiaocatchCommon.mergeHeaders(...)
 */
(function (root) {
  "use strict";

  /** URL 最大长度，超长（多为 base64 / data 参数）直接丢弃，避免撑爆内存 */
  var MAX_URL_LENGTH = 8000;
  /** 单个请求头 value 的最大长度 */
  var MAX_HEADER_VALUE_LENGTH = 4000;
  /** 单次捕获携带的 m3u8 文本上限 */
  var MAX_CONTENT_LENGTH = 50000;

  /**
   * 取基准地址：显式传入的 base 优先，其次全局 location。
   * @param {(string|URL|undefined|null)} base
   * @return {string} 可为空字符串
   */
  function baseHref(base) {
    if (typeof base === "string" && base) return base;
    if (base && typeof base.href === "string" && base.href) return base.href;
    if (typeof location !== "undefined" && location && typeof location.href === "string" && location.href) {
      return location.href;
    }
    return "";
  }

  /**
   * 把任意形态的 URL 归一化为 absolute http/https URL。
   * 支持：字符串（绝对/相对）、URL 实例、带 href 的对象、Request（由调用方取 input.url）。
   * @param {*} raw 原始 URL
   * @param {(string|URL)} [base] 相对地址的基准，缺省用当前页面 location.href
   * @return {string} 归一化后的 URL；非法协议 / 空值 / 超长时返回空字符串
   */
  function normalizeUrl(raw, base) {
    var text = "";
    if (typeof raw === "string") {
      text = raw;
    } else if (raw && typeof raw.href === "string") {
      text = raw.href;
    } else if (raw && typeof raw.url === "string") {
      text = raw.url;
    } else {
      return "";
    }
    text = text.trim();
    if (!text) return "";

    var parsed = null;
    try {
      var origin = baseHref(base);
      parsed = origin ? new URL(text, origin) : new URL(text);
    } catch (e) {
      return "";
    }
    if (!parsed) return "";

    var protocol = String(parsed.protocol || "").toLowerCase();
    if (protocol !== "http:" && protocol !== "https:") return "";

    var href = parsed.href;
    if (!href || href.length > MAX_URL_LENGTH) return "";
    // 下载器侧会对含空白/控制字符的 URL 返回 400，这里直接丢弃
    if (/[\s\u0000-\u001f\u007f]/.test(href)) return "";
    return href;
  }

  /**
   * 判断 URL 是否是 m3u8 / m3u 资源（只看 pathname，忽略 query 和 hash）。
   * @param {string} url
   * @return {boolean}
   */
  function isM3U8Url(url) {
    if (typeof url !== "string" || !url) return false;
    var pathname = "";
    try {
      pathname = new URL(url).pathname || "";
    } catch (e) {
      pathname = url.split("?")[0].split("#")[0];
    }
    var lower = String(pathname).toLowerCase();
    return lower.slice(-5) === ".m3u8" || lower.slice(-4) === ".m3u";
  }

  /**
   * 判断 URL 是否是 DASH 清单（只看 pathname，忽略 query 和 hash）。
   * @param {string} url
   * @return {boolean}
   */
  function isDashUrl(url) {
    if (typeof url !== "string" || !url) return false;
    var pathname = "";
    try {
      pathname = new URL(url).pathname || "";
    } catch (e) {
      pathname = url.split("?")[0].split("#")[0];
    }
    return String(pathname).toLowerCase().slice(-4) === ".mpd";
  }

  /**
   * 从 URL 判断流格式。
   * @param {string} url
   * @return {string} "hls" / "dash" / ""（未知）
   */
  function streamKind(url) {
    if (isM3U8Url(url)) return "hls";
    if (isDashUrl(url)) return "dash";
    return "";
  }

  /**
   * 判断 URL 是否是当前支持的流清单。
   * @param {string} url
   * @return {boolean}
   */
  function isStreamUrl(url) {
    return !!streamKind(url);
  }

  /**
   * 把外部传入的 kind 规整为内部值。
   * 兼容旧消息里的 m3u8/mpd 别名；非法值返回空字符串。
   * @param {*} kind
   * @return {string}
   */
  function normalizeKind(kind) {
    var text = typeof kind === "string" ? kind.trim().toLowerCase() : "";
    if (text === "hls" || text === "m3u8" || text === "m3u") return "hls";
    if (text === "dash" || text === "mpd") return "dash";
    if (text === "page" || text === "web" || text === "url") return "page";
    return "";
  }

  /**
   * 格式的展示名。
   * @param {string} kind
   * @return {string}
   */
  function kindLabel(kind) {
    var k = normalizeKind(kind);
    if (k === "hls") return "HLS";
    if (k === "dash") return "DASH";
    if (k === "page") return "网页";
    return "";
  }

  /**
   * 按响应体内容判断流格式（兜底：很多 CDN 不给正确 content-type 或没有后缀）。
   * @param {*} text
   * @return {string} "hls" / "dash" / ""
   */
  function contentKind(text) {
    if (typeof text !== "string" || !text) return "";
    if (text.indexOf("#EXTM3U") >= 0) return "hls";
    if (/<MPD[\s>]/i.test(text)) return "dash";
    return "";
  }

  /**
   * 写入单个请求头，统一做去空格、小写键、过滤空值与 http2 伪头、限长。
   * @param {!Object<string,string>} out 输出对象
   * @param {*} name 头名
   * @param {*} value 头值
   */
  function addHeader(out, name, value) {
    if (typeof name !== "string") return;
    var key = name.trim().toLowerCase();
    if (!key || key.charAt(0) === ":") return; // 过滤 http2 伪头
    if (value === undefined || value === null) return;
    var text = String(value).trim();
    if (!text) return; // 过滤空值
    if (text.length > MAX_HEADER_VALUE_LENGTH) text = text.slice(0, MAX_HEADER_VALUE_LENGTH);
    out[key] = text;
  }

  /**
   * 把各种形态的请求头转成普通对象（键小写、空值过滤）。
   * 支持 Headers 实例、[[k,v],...]、[{name,value},...]、普通对象。
   * @param {*} headers
   * @return {!Object<string,string>}
   */
  function cleanHeaders(headers) {
    var out = {};
    if (!headers || typeof headers !== "object") return out;

    if (typeof headers.forEach === "function" && typeof headers.get === "function") {
      try {
        headers.forEach(function (value, key) {
          addHeader(out, key, value);
        });
      } catch (e) {
        /* 忽略异常，返回已收集部分 */
      }
      return out;
    }

    if (Array.isArray(headers)) {
      for (var i = 0; i < headers.length; i++) {
        var pair = headers[i];
        if (!pair) continue;
        if (Array.isArray(pair) && pair.length >= 2) addHeader(out, pair[0], pair[1]);
        else if (typeof pair.name === "string") addHeader(out, pair.name, pair.value);
      }
      return out;
    }

    var keys = Object.keys(headers);
    for (var k = 0; k < keys.length; k++) addHeader(out, keys[k], headers[keys[k]]);
    return out;
  }

  /**
   * 合并两批请求头：network（webRequest 抓到的真实请求头）优先，
   * page（页面内 fetch/XHR 拦截抓到的头）只补充 network 缺失的键，空值全部过滤。
   * @param {*} pageHeaders 页面侧抓到的请求头
   * @param {*} networkHeaders 浏览器真实发出的请求头
   * @return {!Object<string,string>} 合并结果（键小写）
   */
  function mergeHeaders(pageHeaders, networkHeaders) {
    var page = cleanHeaders(pageHeaders);
    var network = cleanHeaders(networkHeaders);
    var merged = {};
    var pageKeys = Object.keys(page);
    for (var i = 0; i < pageKeys.length; i++) merged[pageKeys[i]] = page[pageKeys[i]];
    var netKeys = Object.keys(network);
    for (var j = 0; j < netKeys.length; j++) merged[netKeys[j]] = network[netKeys[j]];
    return merged;
  }

  /** 发送给下载器时保留的请求头前缀（其余视为浏览器噪音丢弃） */
  var SEND_HEADER_PREFIXES = ["referer", "origin", "cookie", "authorization", "user-agent", "x-", "token"];

  /** 下载器契约限制：headers 最多 32 个、总长 ≤ 60KB（服务端上限 64KB，留余量） */
  var MAX_SEND_HEADERS = 32;
  var MAX_SEND_HEADER_TOTAL = 60 * 1024;
  /** 越靠前越重要，超出数量上限时优先保留 */
  var SEND_HEADER_PRIORITY = ["cookie", "authorization", "referer", "origin", "user-agent", "token"];

  /**
   * 判断头名是否是合法的 RFC7230 token（页面可能塞入非法头名，服务端会静默丢弃）。
   * @param {string} key 已小写化的头名
   * @return {boolean}
   */
  function isHeaderNameValid(key) {
    if (typeof key !== "string" || !key || key.length > 128) return false;
    return /^[!#$%&'*+\-.^_`|~0-9a-z]+$/.test(key);
  }

  /**
   * 判断头名是否属于需要透传给下载器的白名单。
   * @param {string} key 已小写化的头名
   * @return {boolean}
   */
  function isSendHeaderKey(key) {
    for (var i = 0; i < SEND_HEADER_PREFIXES.length; i++) {
      if (key.indexOf(SEND_HEADER_PREFIXES[i]) === 0) return true;
    }
    return false;
  }

  /**
   * 排序权重：数字越小越优先保留。
   * @param {string} key 已小写化的头名
   * @return {number}
   */
  function headerPriority(key) {
    for (var i = 0; i < SEND_HEADER_PRIORITY.length; i++) {
      if (key.indexOf(SEND_HEADER_PRIORITY[i]) === 0) return i;
    }
    return SEND_HEADER_PRIORITY.length;
  }

  /**
   * 只保留对下载有意义的请求头，过滤浏览器自动加的噪音头与非法头名，
   * 并按下载器契约限制数量（32 个）与总长度（60KB），超限时优先保留关键头。
   * @param {*} headers
   * @return {!Object<string,string>}
   */
  function filterSendHeaders(headers) {
    var source = cleanHeaders(headers);
    var keys = [];
    var allKeys = Object.keys(source);
    for (var i = 0; i < allKeys.length; i++) {
      if (isSendHeaderKey(allKeys[i]) && isHeaderNameValid(allKeys[i])) keys.push(allKeys[i]);
    }
    keys.sort(function (a, b) {
      return headerPriority(a) - headerPriority(b);
    });

    var out = {};
    var total = 0;
    var count = 0;
    for (var k = 0; k < keys.length; k++) {
      if (count >= MAX_SEND_HEADERS) break;
      var key = keys[k];
      var value = source[key];
      var cost = key.length + value.length + 4;
      if (total + cost > MAX_SEND_HEADER_TOTAL) continue;
      out[key] = value;
      total += cost;
      count++;
    }
    return out;
  }

  root.MiaocatchCommon = {
    MAX_URL_LENGTH: MAX_URL_LENGTH,
    MAX_HEADER_VALUE_LENGTH: MAX_HEADER_VALUE_LENGTH,
    MAX_CONTENT_LENGTH: MAX_CONTENT_LENGTH,
    MAX_SEND_HEADERS: MAX_SEND_HEADERS,
    MAX_SEND_HEADER_TOTAL: MAX_SEND_HEADER_TOTAL,
    normalizeUrl: normalizeUrl,
    isM3U8Url: isM3U8Url,
    isDashUrl: isDashUrl,
    isStreamUrl: isStreamUrl,
    streamKind: streamKind,
    normalizeKind: normalizeKind,
    kindLabel: kindLabel,
    contentKind: contentKind,
    cleanHeaders: cleanHeaders,
    mergeHeaders: mergeHeaders,
    filterSendHeaders: filterSendHeaders
  };
})(typeof globalThis !== "undefined" ? globalThis : this);
