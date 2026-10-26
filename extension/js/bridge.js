/**
 * HLSink注入桥接（ISOLATED world）。
 *
 * js/inject.js 运行在页面 MAIN world（只有这样才能看到页面的真实 fetch/XHR 参数），
 * 但 MAIN world 里通常拿不到扩展 API，因此 inject.js 用 window.postMessage 抛结果，
 * 由本脚本（隔离环境，拥有 chrome.* 权限）转发给 background。
 *
 * 只转发白名单类型，并对 payload 做形状校验/限长，避免页面脚本伪造超大数据。
 */
(function () {
  "use strict";

  if (window.__miaocatch_bridge__) return;
  window.__miaocatch_bridge__ = true;

  var BRIDGE_KEY = "__miaocatch__";
  var MAX_CONTENT_LENGTH = 50000;
  var MAX_HEADER_COUNT = 100;
  var MAX_HEADER_VALUE_LENGTH = 4000;
  var ALLOWED_TYPES = { "m3u8-found": true };

  /**
   * 校验并裁剪页面传来的请求头。
   * @param {*} headers
   * @return {!Object<string,string>}
   */
  function sanitizeHeaders(headers) {
    var out = {};
    if (!headers || typeof headers !== "object") return out;
    var keys = Object.keys(headers);
    var count = 0;
    for (var i = 0; i < keys.length && count < MAX_HEADER_COUNT; i++) {
      var name = keys[i];
      if (typeof name !== "string") continue;
      var key = name.trim().toLowerCase();
      if (!key || key.charAt(0) === ":") continue;
      var value = headers[name];
      if (value === undefined || value === null) continue;
      var text = String(value).trim();
      if (!text) continue;
      if (text.length > MAX_HEADER_VALUE_LENGTH) text = text.slice(0, MAX_HEADER_VALUE_LENGTH);
      out[key] = text;
      count++;
    }
    return out;
  }

  /**
   * 把页面消息转成发给 background 的 m3u8-found 消息。
   * @param {*} payload 页面侧 payload
   * @return {?Object} 非法时返回 null
   */
  function toRuntimeMessage(payload) {
    if (!payload || typeof payload !== "object") return null;
    if (!ALLOWED_TYPES[payload.type]) return null;
    if (typeof payload.url !== "string" || !payload.url) return null;

    var message = {
      type: payload.type,
      url: payload.url,
      headers: sanitizeHeaders(payload.headers),
      pageUrl: typeof payload.pageUrl === "string" ? payload.pageUrl : ""
    };
    if (typeof payload.content === "string" && payload.content) {
      message.content = payload.content.slice(0, MAX_CONTENT_LENGTH);
    }
    return message;
  }

  window.addEventListener(
    "message",
    function (event) {
      try {
        if (event.source !== window) return;
        var data = event.data;
        if (!data || typeof data !== "object" || data[BRIDGE_KEY] !== "capture") return;
        var message = toRuntimeMessage(data.payload);
        if (!message) return;
        var pending = chrome.runtime.sendMessage(message);
        if (pending && typeof pending.catch === "function") pending.catch(function () {});
      } catch (e) {
        /* background 可能正在重启，丢弃本次消息即可 */
      }
    },
    false
  );
})();
