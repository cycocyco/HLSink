/* =========================================================================
 * HLSink 前端引导
 *
 * 只在 Wails v3 桌面壳（WebView2）里运行，直接连 Go 后端。
 * 没有预览模式、没有假数据：连不上后端就明确报错，绝不伪装成正常界面。
 * ========================================================================= */
(async () => {
  // WebView2 才会注入 window.chrome.webview，用它判断是否运行在桌面壳里。
  const IN_WEBVIEW = !!(window.chrome && window.chrome.webview);

  let RT = null;
  let runtimeError = null;
  if (IN_WEBVIEW) {
    // 优先用随前端一起打包的运行时副本（普通静态文件，必定 200）。
    // 特殊路由 /wails/runtime.js 在部分资源服务器配置下会拿不到。
    const candidates = ['./vendor/wails-runtime.js', '/wails/runtime.js'];
    for (const url of candidates) {
      try {
        RT = await import(url);
        break;
      } catch (err) {
        runtimeError = err;
      }
    }
  }
  if (!RT) {
    // 只允许在 Wails 桌面壳里运行。拿不到运行时就是环境不对，直接报错，
    // 不做任何假数据兜底——否则用户会把 mock 当成真实状态（比如凭空的缓存体积）。
    console.error('[HLSink] Wails 运行时加载失败', runtimeError);
    const box = document.createElement('div');
    box.className = 'fatal-error';
    box.innerHTML =
      '<h2>请通过 HLSink.exe 启动</h2>' +
      '<p>当前页面没有连上本地后端（前端运行时未加载）。请直接运行 HLSink.exe，不要用浏览器打开 index.html。</p>';
    document.body.appendChild(box);
    return;
  }

  const safe = (p) => Promise.resolve(p).catch((e) => console.error('[HLSink]', e));

  // Wails 绑定的全限定名 = <编译时包的 PkgPath>.App.<方法名>。
  // 本项目用 go build 出 exe，main 包的 PkgPath 就是 "main"，所以是 main.App.Xxx。
  // 注意：go test 里同一个类型的 PkgPath 会变成模块名（hlsink），两者不同，
  // 别拿测试结果当准。验证 exe 内的真实值：HLSink.exe --print-fqn
  const APP_NS = 'main.App.';
  const call = (method, ...args) => RT.Call.ByName(APP_NS + method, ...args);

  const AppApi = {
    ListTasks: () => call('ListTasks'),
    AddTask: (url, title) => call('AddTask', url, title),
    GetSettings: () => call('GetSettings'),
    SaveSettings: (s) => call('SaveSettings', s),
    PauseTask: (id) => call('PauseTask', id),
    ResumeTask: (id) => call('ResumeTask', id),
    DeleteTask: (id) => call('DeleteTask', id),
    PickFolder: () => call('PickFolder'),
    PickToolFolder: () => call('PickToolFolder'),
    ToggleMini: () => call('ToggleMini'),
    ToggleAlwaysOnTop: () => call('ToggleAlwaysOnTop'),
    AlwaysOnTop: () => call('AlwaysOnTop'),
    ClearCache: () => call('ClearCache'),
    CacheInfo: () => call('CacheInfo'),
    CheckTools: () => call('CheckTools'),
    LoginTelegram: () => call('LoginTelegram'),
    ServerStatus: () => call('ServerStatus'),
  };

  const { ListTasks, GetSettings, SaveSettings, PauseTask, ResumeTask, DeleteTask, PickFolder, PickToolFolder, ToggleMini, ToggleAlwaysOnTop, AlwaysOnTop, AddTask, ClearCache, CacheInfo, CheckTools } = AppApi;

  const EventsOn = (name, cb) =>
    RT.Events.On(name, (ev) => cb(ev && typeof ev === 'object' && 'data' in ev ? ev.data : ev));

  const WindowMinimise = () => safe(RT.Window.Minimise());
  const WindowSetSize = (w, h) => safe(RT.Window.SetSize(w, h));
  // 关闭走官方 Window.Close()（原生 WM_CLOSE），不要从绑定调用里调 Application.Quit()：
  // 后者会在 cleanup 里停 HTTP transport，等待当前这个调用结束，形成互相等待。
  // 关闭窗口 = 进托盘（轻量模式）：真正销毁 WebView 释放内存，进程留在后台。
  // 彻底退出走托盘右键菜单，不要再从界面里直接 Quit。
  const Quit = () => safe(RT.Window.Close());

/* =========================================================================
 * 自绘窗口键
 * ========================================================================= */
document.getElementById('win-min').addEventListener('click', () => WindowMinimise());
document.getElementById('win-close').addEventListener('click', () => Quit());

/* =========================================================================
 * 全局状态
 * ========================================================================= */
const state = {
  /** @type {Array<object>} 任务列表（后端顺序：旧 -> 新） */
  tasks: [],
  /** @type {?{downloadDir: string, hevctTranscode: boolean, threadCount: number}} */
  settings: null,
  /** 搜索关键词（已 trim + 小写化前的原值） */
  search: '',
  /** @type {'all'|'active'|'done'|'failed'} */
  filter: 'all',
  /** @type {Set<string>} 有请求在飞的任务 id，用于禁用按钮防双击 */
  busy: new Set(),
  /** @type {Map<string, string>} 乐观状态覆盖：id -> 临时状态（如 deleting / pausing） */
  optStatus: new Map(),
  /** @type {Map<string, string>} 操作失败信息：id -> 文案 */
  actionError: new Map(),
};

/** 默认设置：后端拿不到时兜底，避免空引用 */
const DEFAULT_SETTINGS = { downloadDir: '', hevctTranscode: true, threadCount: 32, rePath: '', ytdlpPath: '', ffmpegPath: '', tdlPath: '' };

/** 任务状态 -> 中文文案 */
const statusText = {
  queued: '排队中',
  running: '下载中',
  pausing: '暂停中',
  transcoding: '转码中',
  deleting: '删除中',
  paused: '已暂停',
  cleanup_failed: '清理中',
  done: '完成',
  error: '失败',
};

/** 已知状态集合：用于拼 class 时做白名单，避免任何非预期字符串进入 innerHTML/class */
const KNOWN_STATUS = new Set(Object.keys(statusText));

/** 服务状态文本元素 */
const PING_URL = 'http://127.0.0.1:18752/ping';
const PING_TIMEOUT_MS = 1500;
const SERVER_PROBE_INTERVAL_MS = 15000;

/* =========================================================================
 * 小工具
 * ========================================================================= */

/**
 * 把任意错误转成可读文案。
 * Wails 绑定 reject 出来的通常是字符串，也可能是 Error。
 * @param {unknown} err
 * @returns {string}
 */
function errMsg(err) {
  if (err === null || err === undefined) return '未知错误';
  if (typeof err === 'string') return err.trim() || '未知错误';
  if (typeof err === 'object' && err !== null && 'message' in err) {
    const m = String(/** @type {{message: unknown}} */ (err).message || '').trim();
    if (m) return m;
  }
  try {
    const s = String(err);
    return s && s !== '[object Object]' ? s : '未知错误';
  } catch (_e) {
    return '未知错误';
  }
}

/**
 * 状态白名单：未知状态统一落到 unknown，避免脏 class。
 * @param {string} status
 * @returns {string}
 */
function safeStatus(status) {
  const s = String(status || '');
  return KNOWN_STATUS.has(s) ? s : 'unknown';
}

/**
 * 取任务的有效状态（本地乐观状态优先于后端快照）。
 * @param {object} t
 * @returns {string}
 */
function effectiveStatus(t) {
  if (!t) return 'unknown';
  const opt = state.optStatus.get(t.id);
  return safeStatus(opt || t.status);
}

/**
 * 引擎名美化。
 * @param {string} engine
 * @returns {string}
 */
function engineLabel(engine) {
  const e = String(engine || '').toLowerCase();
  if (e === 're' || e === 'n_m3u8dl-re') return 'N_m3u8DL-RE';
  if (e === 'ytdlp' || e === 'yt-dlp') return 'yt-dlp';
  return String(engine);
}

/**
 * 源格式展示名。
 * @param {string} kind
 * @returns {string}
 */
function kindLabel(kind) {
  const k = String(kind || '').toLowerCase();
  if (k === 'hls' || k === 'm3u8' || k === 'm3u') return 'HLS';
  if (k === 'dash' || k === 'mpd') return 'DASH';
  if (k === 'page' || k === 'web') return '网页';
  return '';
}

/**
 * 进度百分比收敛到 0..100 且保留一位小数。
 * @param {number} progress
 * @returns {{width: string, text: string}}
 */
function progressInfo(progress) {
  const n = Number(progress);
  const v = Number.isFinite(n) ? Math.min(100, Math.max(0, n)) : 0;
  return { width: v + '%', text: v.toFixed(1) + '%' };
}

/**
 * 创建元素并设置 class。
 * @param {string} tag
 * @param {string} [className]
 * @returns {HTMLElement}
 */
function el(tag, className) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  return node;
}

/* =========================================================================
 * Toast（轻量，无第三方库）
 * ========================================================================= */
const TOAST_LIFETIME_MS = 3600;
const TOAST_MAX = 4;
/** @type {?HTMLElement} */
let toastWrap = null;

/**
 * 确保 toast 容器存在。
 * @returns {HTMLElement}
 */
function ensureToastWrap() {
  if (toastWrap && document.body.contains(toastWrap)) return toastWrap;
  toastWrap = el('div', 'toast-wrap');
  toastWrap.id = 'toast-wrap';
  document.body.appendChild(toastWrap);
  return toastWrap;
}

/**
 * 显示一条提示。
 * @param {string} message 纯文本，不做 HTML 解析
 * @param {'info'|'error'|'ok'} [kind]
 */
function showToast(message, kind = 'info') {
  const text = String(message || '').trim();
  if (!text) return;
  const wrap = ensureToastWrap();
  while (wrap.children.length >= TOAST_MAX) wrap.removeChild(wrap.children[0]);

  const node = el('div', 'toast toast-' + (kind === 'error' ? 'error' : kind === 'ok' ? 'ok' : 'info'));
  node.textContent = text;
  wrap.appendChild(node);

  let removed = false;
  const remove = () => {
    if (removed) return;
    removed = true;
    node.classList.add('toast-out');
    // CSS 里退场是 --dur-leave(200ms)，这里留 50ms 余量再摘节点。
    window.setTimeout(() => {
      if (node.parentNode) node.parentNode.removeChild(node);
    }, 250);
  };
  node.addEventListener('click', remove);
  window.setTimeout(remove, TOAST_LIFETIME_MS);
}

/* =========================================================================
 * 确认弹窗（轻量，无第三方库）
 * ========================================================================= */
/** @type {?{mask: HTMLElement, close: (v: any) => void}} */
let activeDialog = null;

/**
 * 弹出层退场：卡片先收、幕布后淡，播完再摘节点。
 *
 * 关键点是「调用方不等动画」——resolve 已经在动画开始前完成，
 * 业务逻辑立刻继续，用户看到的是动画和后续操作并行。
 * @param {HTMLElement} mask
 */
function dismissModal(mask) {
  if (!mask || mask.dataset.closing === '1') return;
  mask.dataset.closing = '1';
  const reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (reduce) {
    mask.remove();
    return;
  }
  mask.classList.add('is-closing');
  let dropped = false;
  const drop = () => {
    if (dropped) return;
    dropped = true;
    mask.remove();
  };
  mask.addEventListener('animationend', (e) => {
    // 只认退场那条动画。开场的 veil-in 也会在 mask 上触发 animationend，
    // 若用户在开场动画没播完时就关掉，会被误判成「退场结束」而瞬间消失。
    if (e.target === mask && e.animationName === 'veil-out') drop();
  });
  // 兜底：标签页在后台时 animationend 可能不触发
  window.setTimeout(drop, 600);
}

/**
 * 二次确认对话框。
 * @param {{title: string, message: string, confirmText?: string, cancelText?: string, danger?: boolean}} opts
 * @returns {Promise<boolean>} 用户确认返回 true
 */
function confirmDialog(opts) {
  const title = String(opts.title || '请确认');
  const message = String(opts.message || '');
  const confirmText = String(opts.confirmText || '确定');
  const cancelText = String(opts.cancelText || '取消');
  const danger = !!opts.danger;

  // 已有弹窗未关闭时，先按“取消”收掉，避免叠加
  if (activeDialog) activeDialog.close(false);

  return new Promise((resolve) => {
    const mask = el('div', 'modal-mask');
    const box = el('div', 'modal');

    const h = el('div', 'modal-title');
    h.textContent = title;

    const body = el('div', 'modal-body');
    body.textContent = message;

    const actions = el('div', 'modal-actions');
    const cancelBtn = el('button', 'modal-btn');
    cancelBtn.type = 'button';
    cancelBtn.textContent = cancelText;
    const okBtn = el('button', 'modal-btn' + (danger ? ' danger' : ' primary'));
    okBtn.type = 'button';
    okBtn.textContent = confirmText;

    actions.appendChild(cancelBtn);
    actions.appendChild(okBtn);
    box.appendChild(h);
    box.appendChild(body);
    box.appendChild(actions);
    mask.appendChild(box);
    document.body.appendChild(mask);

    let settled = false;
    const close = (value) => {
      if (settled) return;
      settled = true;
      // 先解绑键盘，避免退场期间还能按 Enter 二次触发
      document.removeEventListener('keydown', onKeyDown, true);
      if (activeDialog && activeDialog.mask === mask) activeDialog = null;
      // 先兑现 Promise，业务立刻继续；节点等退场动画播完再摘
      resolve(value);
      dismissModal(mask);
    };
    const onKeyDown = (e) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        close(false);
      } else if (e.key === 'Enter') {
        e.preventDefault();
        close(true);
      }
    };

    cancelBtn.addEventListener('click', () => close(false));
    okBtn.addEventListener('click', () => close(true));
    mask.addEventListener('click', (e) => {
      if (e.target === mask) close(false);
    });
    document.addEventListener('keydown', onKeyDown, true);

    activeDialog = { mask, close };
    // 危险操作默认聚焦“取消”，避免回车误删
    window.setTimeout(() => (danger ? cancelBtn : okBtn).focus(), 0);
  });
}

/**
 * 单行输入弹窗（用于手动添加链接）。
 * @param {{title: string, message: string, placeholder?: string, confirmText?: string, cancelText?: string, initial?: string}} opts
 * @returns {Promise<?string>} 取消返回 null，确认返回已 trim 的输入
 */
function inputDialog(opts) {
  const title = String(opts.title || '请输入');
  const message = String(opts.message || '');
  const placeholder = String(opts.placeholder || '');
  const confirmText = String(opts.confirmText || '确定');
  const cancelText = String(opts.cancelText || '取消');

  if (activeDialog) activeDialog.close(null);

  return new Promise((resolve) => {
    const mask = el('div', 'modal-mask');
    const box = el('div', 'modal');

    const h = el('div', 'modal-title');
    h.textContent = title;

    const body = el('div', 'modal-body');
    body.textContent = message;

    const input = /** @type {HTMLInputElement} */ (el('input', 'modal-input'));
    input.type = 'text';
    input.spellcheck = false;
    input.autocomplete = 'off';
    input.placeholder = placeholder;
    input.value = String(opts.initial || '');

    const actions = el('div', 'modal-actions');
    const cancelBtn = el('button', 'modal-btn');
    cancelBtn.type = 'button';
    cancelBtn.textContent = cancelText;
    const okBtn = el('button', 'modal-btn primary');
    okBtn.type = 'button';
    okBtn.textContent = confirmText;

    actions.appendChild(cancelBtn);
    actions.appendChild(okBtn);
    box.appendChild(h);
    box.appendChild(body);
    box.appendChild(input);
    box.appendChild(actions);
    mask.appendChild(box);
    document.body.appendChild(mask);

    let settled = false;
    const close = (value) => {
      if (settled) return;
      settled = true;
      // 先解绑键盘，避免退场期间还能按 Enter 二次触发
      document.removeEventListener('keydown', onKeyDown, true);
      if (activeDialog && activeDialog.mask === mask) activeDialog = null;
      // 先兑现 Promise，业务立刻继续；节点等退场动画播完再摘
      resolve(value);
      dismissModal(mask);
    };
    const submit = () => {
      const text = input.value.trim();
      if (!text) {
        input.focus();
        return;
      }
      close(text);
    };
    const onKeyDown = (e) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        close(null);
      } else if (e.key === 'Enter') {
        e.preventDefault();
        submit();
      }
    };

    cancelBtn.addEventListener('click', () => close(null));
    okBtn.addEventListener('click', submit);
    mask.addEventListener('click', (e) => {
      if (e.target === mask) close(null);
    });
    document.addEventListener('keydown', onKeyDown, true);

    activeDialog = { mask, close };
    window.setTimeout(() => input.focus(), 0);
  });
}

/* =========================================================================
 * 任务列表渲染
 * ========================================================================= */

/**
 * 任务分组：用于顶部筛选与数量统计。
 * @param {object} t
 * @returns {'active'|'done'|'failed'}
 */
function taskGroup(t) {
  const status = effectiveStatus(t);
  if (status === 'done') return 'done';
  if (status === 'error' || status === 'cleanup_failed') return 'failed';
  return 'active';
}

/**
 * 按搜索词和筛选条件过滤任务。
 * @returns {Array<object>}
 */
function visibleTasks() {
  const q = state.search.trim().toLowerCase();
  let result = state.tasks;
  if (state.filter !== 'all') {
    result = result.filter((t) => taskGroup(t) === state.filter);
  }
  if (!q) return result;
  return result.filter((t) => {
    const title = String(t.title || '').toLowerCase();
    const filename = String(t.filename || '').toLowerCase();
    const url = String(t.url || '').toLowerCase();
    return title.includes(q) || filename.includes(q) || url.includes(q);
  });
}

/**
 * 设置空列表提示（textContent，避免注入）。
 * @param {string} title
 * @param {string} [sub]
 */
function setEmptyTip(title, sub) {
  const box = document.getElementById('empty-tip');
  if (!box) return;
  const titleEl = box.querySelector('.empty-title');
  const subEl = box.querySelector('.empty-sub');
  if (titleEl && subEl) {
    titleEl.textContent = title;
    subEl.textContent = sub || '';
    subEl.style.display = sub ? '' : 'none';
    return;
  }
  box.textContent = '';
  box.appendChild(document.createTextNode(title));
  if (sub) {
    box.appendChild(document.createElement('br'));
    const span = document.createElement('span');
    span.textContent = sub;
    box.appendChild(span);
  }
}

/**
 * 创建任务操作按钮。
 * @param {{act: string, label: string, title: string, danger?: boolean, disabled?: boolean}} opts
 * @returns {HTMLButtonElement}
 */
function actionButton(opts) {
  const btn = /** @type {HTMLButtonElement} */ (el('button', 'tbtn' + (opts.danger ? ' danger' : '')));
  btn.type = 'button';
  btn.dataset.act = opts.act;
  btn.title = opts.title;
  btn.textContent = opts.label;
  btn.disabled = !!opts.disabled;
  return btn;
}

/**
 * 创建一张任务卡片的固定骨架。
 *
 * 结构一次建好、此后永不变动，只由 updateCard 改内容。
 * 这是「按 id 复用节点」的前提：节点不销毁，CSS 动画才不会反复重播。
 * 所有可能出现的段落（路径 / 警告 / 错误）都先建好，用 display 控制显隐，
 * 避免后续插入删除导致结构差异。
 * @returns {HTMLElement}
 */
function createCard() {
  const card = el('div', 'task-card');

  const head = el('div', 'task-head');
  const mark = el('span', 'task-status-mark');
  const title = el('span', 'task-title');
  const statusEl = el('span', 'task-status');
  const actions = el('div', 'task-actions');
  head.appendChild(mark);
  head.appendChild(title);
  head.appendChild(statusEl);
  head.appendChild(actions);

  const sub = el('div', 'task-sub');
  const subLeft = el('span', 'task-sub-left');
  const subRight = el('span', 'task-sub-right');
  sub.appendChild(subLeft);
  sub.appendChild(subRight);

  const path = el('div', 'task-path');
  const warnBox = el('div', 'task-warn');
  const errBox = el('div', 'task-err');

  const bar = el('div', 'task-bar');
  const fill = el('div', 'task-bar-fill');
  bar.appendChild(fill);

  card.appendChild(head);
  card.appendChild(sub);
  card.appendChild(path);
  card.appendChild(warnBox);
  card.appendChild(errBox);
  card.appendChild(bar);
  return card;
}

/**
 * 把任务快照写进卡片骨架。
 *
 * 只改文本、class 和可见性，绝不替换节点本身；写之前先比对，避免无谓的
 * DOM 写入（高频进度刷新时这一点很关键，否则整段文本每帧都在重排）。
 * @param {HTMLElement} card
 * @param {object} t
 */
function updateCard(card, t) {
  const id = t.id;
  const status = effectiveStatus(t);
  const safe = safeStatus(status);
  const busy = state.busy.has(id);
  const locked = busy || status === 'deleting' || status === 'pausing';
  const info = progressInfo(t.progress);
  const label = statusText[safe] || safe;

  // 状态类逐个 toggle，不能用 className 整体覆盖：
  // 否则会把正在播放的 is-entering / is-leaving 一起冲掉。
  card.classList.toggle('is-deleting', status === 'deleting');
  card.classList.toggle('is-failed', status === 'cleanup_failed');
  // 只有真正在跑的（下载/转码）才让进度条流光，暂停和排队时保持静止
  card.classList.toggle('is-transferring', status === 'running' || status === 'transcoding');

  const mark = /** @type {HTMLElement} */ (card.querySelector('.task-status-mark'));
  const markClass = 'task-status-mark s-' + safe;
  if (mark.className !== markClass) mark.className = markClass;
  mark.title = label;
  mark.setAttribute('aria-label', label);

  const title = /** @type {HTMLElement} */ (card.querySelector('.task-title'));
  const fullName = String(t.title || t.filename || '未命名任务');
  if (title.textContent !== fullName) title.textContent = fullName;
  title.title = t.url ? fullName + '\n' + String(t.url) : fullName;

  const statusEl = /** @type {HTMLElement} */ (card.querySelector('.task-status'));
  const statusClass = 'task-status s-' + safe;
  if (statusEl.className !== statusClass) statusEl.className = statusClass;
  const showPercent = status === 'running' || status === 'transcoding' || status === 'paused';
  const statusLine = showPercent ? label + ' ' + info.text : label;
  if (statusEl.textContent !== statusLine) statusEl.textContent = statusLine;

  // 操作按钮的组合只取决于状态与 busy，用签名判断要不要重建。
  // 按钮最多两个，重建比逐个比对增删更不容易出错。
  const actions = /** @type {HTMLElement} */ (card.querySelector('.task-actions'));
  const sig = status + '|' + (busy ? 'b' : '');
  if (actions.dataset.sig !== sig) {
    actions.dataset.sig = sig;
    actions.replaceChildren();
    if (status === 'running' || status === 'queued') {
      actions.appendChild(actionButton({
        act: 'pause',
        label: '暂停',
        title: busy ? '暂停请求处理中…' : '暂停下载',
        disabled: locked,
      }));
    }
    if (status === 'paused') {
      actions.appendChild(actionButton({
        act: 'resume',
        label: '继续',
        title: busy ? '继续请求处理中…' : '继续下载',
        disabled: locked,
      }));
    }
    if (status === 'cleanup_failed') {
      actions.appendChild(actionButton({
        act: 'delete',
        label: '重试清理',
        title: busy ? '正在重试删除…' : '重试删除（再次清理残留文件）',
        danger: true,
        disabled: busy,
      }));
    } else if (status !== 'deleting' && status !== 'pausing') {
      actions.appendChild(actionButton({
        act: 'delete',
        label: '删除',
        title: '永久删除（缓存、分片、原片、成品和日志）',
        danger: true,
        disabled: locked,
      }));
    }
  }

  /* ---- 元信息：引擎 / 分片 / 实时速度 ---- */
  const leftParts = [];
  const kindName = kindLabel(t.kind);
  if (kindName) leftParts.push(kindName);
  if (t.engine) leftParts.push(engineLabel(t.engine));
  if (t.segments) leftParts.push(String(t.segments) + ' 片');
  const rightParts = [];
  if (status === 'running' && t.speed) rightParts.push(String(t.speed));
  if (status === 'transcoding') rightParts.push('转码中');

  const sub = /** @type {HTMLElement} */ (card.querySelector('.task-sub'));
  const subLeft = /** @type {HTMLElement} */ (sub.querySelector('.task-sub-left'));
  const subRight = /** @type {HTMLElement} */ (sub.querySelector('.task-sub-right'));
  const leftText = leftParts.join(' · ');
  const rightText = rightParts.join(' · ');
  if (subLeft.textContent !== leftText) subLeft.textContent = leftText;
  if (subRight.textContent !== rightText) subRight.textContent = rightText;
  sub.style.display = leftParts.length || rightParts.length ? '' : 'none';

  /* ---- 输出路径（成功任务）---- */
  const outPath = t.outputPath ? String(t.outputPath) : status === 'done' && t.message ? String(t.message) : '';
  const path = /** @type {HTMLElement} */ (card.querySelector('.task-path'));
  if (outPath) {
    if (path.textContent !== outPath) path.textContent = outPath;
    path.title = outPath;
    path.style.display = '';
  } else {
    path.style.display = 'none';
  }

  /* ---- 成功但带警告（如转码失败已保留原画） ---- */
  const doneWarn =
    status === 'done' && t.message && String(t.message) !== outPath ? String(t.message) : '';
  const warnBox = /** @type {HTMLElement} */ (card.querySelector('.task-warn'));
  if (doneWarn) {
    if (warnBox.textContent !== doneWarn) warnBox.textContent = doneWarn;
    warnBox.title = doneWarn;
    warnBox.style.display = '';
  } else {
    warnBox.style.display = 'none';
  }

  /* ---- 错误信息：本地操作错误 / 后端 error 字段 / message ---- */
  const localErr = state.actionError.get(id) || '';
  const serverErr = t.error ? String(t.error) : '';
  const messageErr = status === 'error' || status === 'cleanup_failed' ? String(t.message || '') : '';
  // cleanup_failed 时优先展示后端给的短文案（"文件被占用，正在后台清理"），
  // 那条 unlink 长报错只放到 title 里，别糊在卡片上。
  let errText = localErr || messageErr || serverErr;
  if (!errText && status === 'cleanup_failed') errText = '文件被占用，正在后台清理';
  const errBox = /** @type {HTMLElement} */ (card.querySelector('.task-err'));
  if (errText) {
    if (errBox.textContent !== errText) errBox.textContent = errText;
    errBox.title = status === 'cleanup_failed' && serverErr ? serverErr : errText;
    errBox.style.display = '';
  } else {
    errBox.style.display = 'none';
  }

  /* ---- 进度条 ---- */
  const fill = /** @type {HTMLElement} */ (card.querySelector('.task-bar-fill'));
  let fillClass = 'task-bar-fill';
  if (status === 'done') fillClass += ' done';
  else if (status === 'paused') fillClass += ' paused';
  else if (status === 'error' || status === 'cleanup_failed') fillClass += ' failed';
  if (fill.className !== fillClass) fill.className = fillClass;
  if (fill.style.width !== info.width) fill.style.width = info.width;
}

/**
 * 新卡片入场：只在这一张确实是新增时调用，因此动画必然只播一次。
 * @param {HTMLElement} card
 */
function enterCard(card) {
  card.classList.add('is-entering');
  const clear = () => card.classList.remove('is-entering');
  card.addEventListener('animationend', function onEnterEnd(e) {
    if (e.target !== card) return;
    card.removeEventListener('animationend', onEnterEnd);
    clear();
  });
  // 兜底：动画被降级成 none、或标签页切到后台导致 animationend 不触发时，
  // class 会一直挂着。400ms 后无条件摘掉（动画本身只有 220ms）。
  window.setTimeout(clear, 400);
}

/**
 * 任务被真正删除：先把退场动画放完再摘节点。
 *
 * 有 600ms 兜底定时器，防止动效被系统降级（reduced-motion 下动画时长为
 * 0.01ms 或直接 none）时 animationend 不触发，把节点永久留在 DOM 里。
 * @param {HTMLElement} card
 */
function leaveCard(card) {
  if (card.dataset.leaving === '1') return;
  card.dataset.leaving = '1';
  card.classList.add('is-leaving');
  const drop = () => card.remove();
  card.addEventListener('animationend', function onLeaveEnd(e) {
    // 只看 card-out：刚入场就被删时，card-in 的结束事件会先冒上来
    if (e.target !== card || e.animationName !== 'card-out') return;
    card.removeEventListener('animationend', onLeaveEnd);
    drop();
  });
  window.setTimeout(drop, 600);
}

/**
 * 列表的点击委托：卡片会被复用，「构造时绑定闭包」会捕获过期的任务快照，
 * 所以统一在这里按 data-id 取当前数据。
 * @param {HTMLElement} list
 */
let taskListDelegated = false;
function ensureTaskListDelegation(list) {
  if (taskListDelegated) return;
  taskListDelegated = true;
  list.addEventListener('click', (e) => {
    const btn = /** @type {HTMLElement} */ (e.target).closest('.tbtn');
    if (!btn || !list.contains(btn)) return;
    const card = btn.closest('.task-card');
    if (!card) return;
    const id = card.dataset.id || '';
    const t = state.tasks.find((x) => x.id === id);
    if (!t) return;
    e.stopPropagation();
    const act = btn.dataset.act;
    if (act === 'pause') void handleTaskAction(t, 'pause');
    else if (act === 'resume') void handleTaskAction(t, 'resume');
    else if (act === 'delete') void handleDelete(t);
  });
}

/** 更新筛选数量、当前摘要和筛选按钮选中态。 */
function updateTaskOverview() {
  const counts = { all: state.tasks.length, active: 0, done: 0, failed: 0 };
  for (const t of state.tasks) counts[taskGroup(t)]++;

  for (const key of ['all', 'active', 'done', 'failed']) {
    const countEl = document.getElementById('filter-count-' + key);
    if (countEl) countEl.textContent = String(counts[key]);
  }
  document.querySelectorAll('.filter').forEach((btn) => {
    const on = /** @type {HTMLElement} */ (btn).dataset.filter === state.filter;
    btn.classList.toggle('active', on);
    btn.setAttribute('aria-pressed', on ? 'true' : 'false');
  });

  const running = state.tasks.filter((t) => effectiveStatus(t) === 'running').length;
  const queued = state.tasks.filter((t) => effectiveStatus(t) === 'queued').length;
  const summary = document.getElementById('task-summary');
  const summarySub = document.getElementById('task-summary-sub');
  if (summary) {
    if (!state.tasks.length) summary.textContent = '暂无任务';
    else if (running || queued) summary.textContent = `${running} 个下载中 · ${queued} 个排队`;
    else if (counts.active) summary.textContent = `${counts.active} 个未完成`;
    else summary.textContent = '当前没有下载任务';
  }
  if (summarySub) {
    summarySub.textContent = state.tasks.length ? `共 ${state.tasks.length} 个任务` : '';
  }
}

/** 渲染任务列表（全量重绘，卡片数量级很小，逻辑简单不易出错） */
function renderTasks() {
  const list = document.getElementById('task-list');
  if (!list) return;
  ensureTaskListDelegation(list);

  const visible = visibleTasks();
  const alive = new Set(state.tasks.map((t) => t.id));
  const wanted = new Set(visible.map((t) => t.id));

  /* 1) 收集现有卡片。正在退场的不参与复用，等它自己播完摘掉。 */
  /** @type {Map<string, HTMLElement>} */
  const existing = new Map();
  list.querySelectorAll('.task-card').forEach((n) => {
    const id = n.dataset.id;
    if (id && n.dataset.leaving !== '1') existing.set(id, n);
  });

  /* 2) 不再展示的卡片：任务真的没了才播退场；只是被筛选掉就直接摘掉，
        否则切个筛选也会满屏动画。 */
  for (const [id, node] of existing) {
    if (wanted.has(id)) continue;
    existing.delete(id);
    if (alive.has(id)) node.remove();
    else leaveCard(node);
  }

  /* 3) 复用或新建，按数据顺序排好 */
  const ordered = [];
  for (const t of visible) {
    let card = existing.get(t.id);
    if (card) {
      updateCard(card, t);
    } else {
      card = createCard();
      card.dataset.id = t.id;
      updateCard(card, t);
      enterCard(card);
    }
    ordered.push(card);
  }

  /* 4) 顺序校正：只在真的错位时才 insertBefore。
        无差别地重新 append 会让节点脱离文档，正在播放的动画会被打断重来。 */
  const skipLeaving = (node) => {
    let n = node;
    while (n && n.dataset && n.dataset.leaving === '1') n = n.nextElementSibling;
    return n;
  };
  let anchor = /** @type {?HTMLElement} */ (document.getElementById('empty-tip'));
  for (const card of ordered) {
    const next = skipLeaving(anchor ? anchor.nextElementSibling : list.firstElementChild);
    const atEnd = next === null && card === list.lastElementChild;
    if (next !== card && !atEnd) list.insertBefore(card, next);
    anchor = card;
  }

  /* 5) 空状态提示 */
  const empty = document.getElementById('empty-tip');
  if (empty) {
    if (!state.tasks.length) {
      setEmptyTip('暂无任务', '在浏览器里播放视频，或点「+ 链接」粘贴网页地址');
    } else if (!visible.length) {
      if (state.search.trim()) {
        setEmptyTip('无匹配任务', '试试别的关键词或切换筛选');
      } else {
        const labels = { active: '进行中', done: '已完成', failed: '失败' };
        setEmptyTip('没有' + (labels[state.filter] || '') + '任务', '切换上方筛选查看其他任务');
      }
    }
    empty.style.display = visible.length ? 'none' : '';
  }

  updateTaskOverview();
}

/* =========================================================================
 * 任务操作
 * ========================================================================= */

/**
 * 统一的任务操作入口：busy 防双击 + 乐观状态 + 失败回滚 + 错误提示。
 * @param {object} t
 * @param {'pause'|'resume'} act
 */
async function handleTaskAction(t, act) {
  const id = t.id;
  if (state.busy.has(id)) return;
  if (effectiveStatus(t) === 'deleting') return;

  state.busy.add(id);
  state.actionError.delete(id);
  if (act === 'pause') state.optStatus.set(id, 'pausing');
  else state.optStatus.delete(id);
  refresh();

  try {
    if (act === 'pause') {
      await PauseTask(id);
    } else {
      await ResumeTask(id);
    }
  } catch (err) {
    console.error('[task] ' + act + ' 失败', id, err);
    const msg = (act === 'pause' ? '暂停失败：' : '继续失败：') + errMsg(err);
    state.actionError.set(id, msg);
    showToast(msg, 'error');
  } finally {
    state.busy.delete(id);
    state.optStatus.delete(id);
    refresh();
    scheduleReconcile();
  }
}

/**
 * 删除任务：二次确认 -> 调 DeleteTask（物理删除）。
 * 失败或收到 cleanup_failed 时恢复卡片并提示，绝不让它看起来“删成功”。
 * @param {object} t
 */
async function handleDelete(t) {
  const id = t.id;
  if (state.busy.has(id)) return;

  const name = String(t.title || t.filename || '该任务');
  const ok = await confirmDialog({
    title: '永久删除任务',
    message:
      '将永久删除该任务的缓存、分片、原片、转码成品和日志，不可恢复；不会删除其他文件。\n\n任务：' +
      name,
    confirmText: '永久删除',
    cancelText: '取消',
    danger: true,
  });
  if (!ok) return;

  state.busy.add(id);
  state.actionError.delete(id);
  state.optStatus.set(id, 'deleting');
  refresh();

  try {
    await DeleteTask(id);
  } catch (err) {
    console.error('[task] delete 失败', id, err);
    const msg = '删除失败：' + errMsg(err);
    state.actionError.set(id, msg);
    showToast(msg, 'error');
    // 恢复卡片：清掉乐观状态，让后端快照（可能是 cleanup_failed）重新生效
    state.optStatus.delete(id);
    state.busy.delete(id);
    refresh();
    return;
  }

  // 成功：保留“删除中”乐观态，等 task-removed 或对账结果，避免闪回旧状态
  state.busy.delete(id);
  scheduleReconcile();
  refresh();
}

/* =========================================================================
 * 与后端对账
 * ========================================================================= */
/** @type {number} */
let reconcileTimer = 0;

/** 延迟拉一次 ListTasks，纠正事件丢失造成的界面失真 */
function scheduleReconcile() {
  if (reconcileTimer) window.clearTimeout(reconcileTimer);
  reconcileTimer = window.setTimeout(async () => {
    reconcileTimer = 0;
    try {
      const list = await ListTasks();
      if (Array.isArray(list)) {
        state.tasks = list;
        // 后端快照是权威的：清掉残留的乐观状态（busy 只由发起请求的一方释放，保住防双击）
        state.optStatus.clear();
        pruneTransient();
        refresh();
      }
    } catch (err) {
      console.error('[reconcile] 拉取任务列表失败', err);
    }
  }, 900);
}

/** 清掉已不存在任务的本地临时状态，防 map 无限增长 */
function pruneTransient() {
  const alive = new Set(state.tasks.map((t) => t.id));
  for (const id of Array.from(state.busy)) if (!alive.has(id)) state.busy.delete(id);
  for (const id of Array.from(state.optStatus.keys())) {
    if (!alive.has(id)) state.optStatus.delete(id);
  }
  for (const id of Array.from(state.actionError.keys())) {
    if (!alive.has(id)) state.actionError.delete(id);
  }
}

/* =========================================================================
 * 设置页
 * ========================================================================= */

/** 把 state.settings 回填到控件 */
function renderSettings() {
  const s = state.settings || DEFAULT_SETTINGS;
  const dirEl = /** @type {?HTMLInputElement} */ (document.getElementById('download-dir'));
  const hevcEl = /** @type {?HTMLInputElement} */ (document.getElementById('hevc-toggle'));
  const threadEl = /** @type {?HTMLInputElement} */ (document.getElementById('thread-count'));
  const reEl = /** @type {?HTMLInputElement} */ (document.getElementById('re-path'));
  const ytEl = /** @type {?HTMLInputElement} */ (document.getElementById('ytdlp-path'));
  const ffEl = /** @type {?HTMLInputElement} */ (document.getElementById('ffmpeg-path'));
  const tdlEl = /** @type {?HTMLInputElement} */ (document.getElementById('tdl-path'));
  // 正在编辑时不覆盖输入框，避免打断用户输入
  if (dirEl && document.activeElement !== dirEl) dirEl.value = String(s.downloadDir || '');
  if (hevcEl) hevcEl.checked = !!s.hevctTranscode;
  if (threadEl && document.activeElement !== threadEl) {
    threadEl.value = String(clampThreadCount(s.threadCount));
  }
  if (reEl && document.activeElement !== reEl) reEl.value = String(s.rePath || '');
  if (ytEl && document.activeElement !== ytEl) ytEl.value = String(s.ytdlpPath || '');
  if (ffEl && document.activeElement !== ffEl) ffEl.value = String(s.ffmpegPath || '');
  if (tdlEl && document.activeElement !== tdlEl) tdlEl.value = String(s.tdlPath || '');
}

/**
 * 保存中禁用设置控件，防重复提交。
 * 只锁开关，不锁目录输入框：input 被 disabled 会触发 blur，可能引发二次保存。
 */
/**
 * 刷新三个外部工具的检测结果。
 *
 * 后端把用户填的目录/exe 一路解析到具体文件，这里只负责显示：
 * 命中就回显真实路径，没命中就说清楚在哪个位置没找到。
 */
async function refreshToolStates() {
  if (typeof CheckTools !== 'function') return;
  let list = [];
  try {
    list = await CheckTools();
  } catch (err) {
    console.error('[tools] 检测失败', err);
    return;
  }
  if (!Array.isArray(list)) return;
  for (const t of list) {
    const box = document.getElementById('state-' + t.key);
    if (!box) continue;
    const ok = !!t.found;
    box.classList.toggle('bad', !ok);
    const text = (ok ? '✓ ' : '✗ ') + (ok ? t.path : (t.note || '未找到'));
    // 只在内容真的变了时播一次进场：每次切到设置页都闪一遍反而烦。
    if (box.textContent !== text) {
      box.textContent = text;
      box.classList.remove('is-fresh');
      void box.offsetWidth; // 强制回流，让同名动画能重播
      box.classList.add('is-fresh');
    }
    box.title = ok ? t.path : (t.path ? '查找位置：' + t.path : '');
  }
}

function setSettingsBusy(busy) {
  const hevc = /** @type {?HTMLInputElement} */ (document.getElementById('hevc-toggle'));
  const threads = /** @type {?HTMLInputElement} */ (document.getElementById('thread-count'));
  if (hevc) hevc.disabled = !!busy;
  if (threads) threads.disabled = !!busy;
}

/** 下载线程数与后端保持 1～64 的同一范围。 */
function clampThreadCount(value) {
  const n = Number.parseInt(String(value), 10);
  if (!Number.isFinite(n)) return 32;
  return Math.max(1, Math.min(64, n));
}

/** @type {boolean} 设置保存中标记，防止并发保存互相覆盖 */
let settingsSaving = false;

/**
 * 修改并保存设置；失败则回滚控件并提示。
 * @param {(s: {downloadDir: string, hevctTranscode: boolean, threadCount: number}) => void} mutate
 * @returns {Promise<boolean>} 是否保存成功
 */
async function applySettings(mutate) {
  if (settingsSaving) return false;
  settingsSaving = true;

  const prev = {
    downloadDir: String((state.settings || DEFAULT_SETTINGS).downloadDir || ''),
    hevctTranscode: !!(state.settings || DEFAULT_SETTINGS).hevctTranscode,
    threadCount: clampThreadCount((state.settings || DEFAULT_SETTINGS).threadCount),
    rePath: String((state.settings || DEFAULT_SETTINGS).rePath || ''),
    ytdlpPath: String((state.settings || DEFAULT_SETTINGS).ytdlpPath || ''),
    ffmpegPath: String((state.settings || DEFAULT_SETTINGS).ffmpegPath || ''),
    tdlPath: String((state.settings || DEFAULT_SETTINGS).tdlPath || ''),
  };
  const next = {
    downloadDir: prev.downloadDir,
    hevctTranscode: prev.hevctTranscode,
    threadCount: prev.threadCount,
    rePath: prev.rePath,
    ytdlpPath: prev.ytdlpPath,
    ffmpegPath: prev.ffmpegPath,
    tdlPath: prev.tdlPath,
  };
  mutate(next);

  setSettingsBusy(true);
  try {
    // 字段名与后端保持一致：hevctTranscode（勿改名）
    const saved = await SaveSettings({
      downloadDir: next.downloadDir,
      hevctTranscode: next.hevctTranscode,
      threadCount: next.threadCount,
      rePath: next.rePath,
      ytdlpPath: next.ytdlpPath,
      ffmpegPath: next.ffmpegPath,
      tdlPath: next.tdlPath,
    });
    state.settings = saved && typeof saved === 'object' ? saved : next;
    // 保存即重新检测：路径填得对不对，这里立刻能看到
    void refreshToolStates();
    return true;
  } catch (err) {
    console.error('[settings] 保存失败', err);
    state.settings = prev; // 回滚
    showToast('设置保存失败：' + errMsg(err), 'error');
    return false;
  } finally {
    settingsSaving = false;
    setSettingsBusy(false);
    renderSettings();
  }
}

/* =========================================================================
 * 顶部 Tab
 * ========================================================================= */
const tabBar = document.querySelector('.tabs');
const tabThumb = tabBar ? tabBar.querySelector('.tabs-thumb') : null;

/**
 * 把滑块对齐到当前激活的 Tab。
 * @param {boolean} animate 首次布局和窗口缩放传 false，避免滑块从左边"滑进来"
 */
function syncTabThumb(animate) {
  if (!tabBar || !tabThumb) return;
  const active = tabBar.querySelector('.tab.active');
  if (!active) return;

  if (!animate) tabThumb.style.transition = 'none';
  tabThumb.style.width = active.offsetWidth + 'px';
  tabThumb.style.transform = 'translateX(' + active.offsetLeft + 'px)';
  if (!animate) {
    // 读一次布局，让「瞬时定位」和「恢复过渡」落在两帧上，否则会被合并成一次跳变
    void tabThumb.offsetWidth;
    tabThumb.style.transition = '';
  }
}

document.querySelectorAll('.tab').forEach((btn) => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.tab').forEach((b) => b.classList.remove('active'));
    document.querySelectorAll('.page').forEach((p) => p.classList.remove('active'));
    btn.classList.add('active');
    const page = document.getElementById('page-' + btn.dataset.tab);
    if (page) page.classList.add('active');
    // 滑块先起步，页面切换的副作用（探活 / 读缓存）随后
    syncTabThumb(true);
    if (btn.dataset.tab === 'settings') {
      void probeServer();
      void refreshCacheInfo();
      void refreshToolStates();
    }
  });
});

// 首帧定位无动画；迷你模式切换会改窗口尺寸，缩放时自愈一次
syncTabThumb(false);
window.addEventListener('resize', () => syncTabThumb(false));

/* =========================================================================
 * 搜索
 * ========================================================================= */
const searchEl = document.getElementById('search');
if (searchEl) {
  searchEl.addEventListener('input', (e) => {
    state.search = String(/** @type {HTMLInputElement} */ (e.target).value || '');
    renderTasks();
  });
}

/** 切筛选时列表错落淡入的定时器（用于下一次切换前撤掉上一轮 class）。 */
let listFlashTimer = 0;

/**
 * 让当前可见的卡片错落淡入一次。
 *
 * 为什么需要手动触发：卡片节点是 keyed diff 复用的，切筛选只是「隐藏/显示」，
 * 不会重新创建节点，所以 CSS 里没有天然的进场时机。这里打一个一次性 class，
 * 由 CSS 的 nth-child 决定每一项的延迟，播完就摘掉。
 */
function flashTaskList() {
  const list = document.getElementById('task-list');
  if (!list) return;
  if (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
  // 延迟写进 CSS 变量，每项 +22ms，第 8 项之后不再叠加（长列表不能越切越慢）
  const cards = list.querySelectorAll('.task-card');
  cards.forEach((card, i) => {
    const step = Math.min(i, 8) * 22;
    card.style.setProperty('--stagger', step + 'ms');
  });
  list.classList.remove('is-switching');
  void list.offsetWidth; // 强制回流，同名动画才允许重播
  list.classList.add('is-switching');
  window.clearTimeout(listFlashTimer);
  // 最慢一项：280ms 动画 + 176ms 延迟，留 60ms 余量
  listFlashTimer = window.setTimeout(() => {
    list.classList.remove('is-switching');
    cards.forEach((card) => card.style.removeProperty('--stagger'));
  }, 520);
}

document.querySelectorAll('.filter').forEach((btn) => {
  btn.addEventListener('click', () => {
    const value = String(/** @type {HTMLElement} */ (btn).dataset.filter || 'all');
    if (!['all', 'active', 'done', 'failed'].includes(value)) return;
    if (state.filter === value) return; // 点同一个筛选不重播动画
    state.filter = /** @type {'all'|'active'|'done'|'failed'} */ (value);
    renderTasks();
    flashTaskList();
  });
});

/* =========================================================================
 * 添加链接（网页地址 / DASH / HLS / Telegram）
 * ========================================================================= */

/**
 * 调用后端的 AddTask。具体交给哪个引擎由后端按链接判断，前端不做分派。
 * @param {string} url
 * @param {string} [title]
 */
async function addTaskByURL(url, title) {
  return AddTask(url, title || '');
}

/** 弹出输入对话框并创建任务（网页地址交给 yt-dlp 解析）。 */
async function openAddLinkDialog() {
  const value = await inputDialog({
    title: '添加链接',
    message: '支持视频网页地址（yt-dlp 解析）、.mpd / .m3u8 流地址，以及 Telegram 消息链接（t.me/频道/消息ID）。',
    placeholder: 'https://',
    confirmText: '添加',
    cancelText: '取消',
  });
  if (value === null) return;

  const url = String(value).trim();
  if (!url) return;
  if (!/^https?:\/\//i.test(url)) {
    showToast('只支持 http / https 地址', 'error');
    return;
  }

  try {
    await addTaskByURL(url, '');
    showToast('已添加任务', 'ok');
    scheduleReconcile();
  } catch (err) {
    console.error('[add-link] 添加任务失败', err);
    showToast('添加失败：' + errMsg(err), 'error');
  }
}

const addLinkBtn = /** @type {?HTMLButtonElement} */ (document.getElementById('add-link'));
if (addLinkBtn) addLinkBtn.addEventListener('click', () => void openAddLinkDialog());

/* =========================================================================
 * 设置事件绑定
 * ========================================================================= */
const hevcToggle = document.getElementById('hevc-toggle');
if (hevcToggle) {
  hevcToggle.addEventListener('change', async (e) => {
    const checked = !!/** @type {HTMLInputElement} */ (e.target).checked;
    await applySettings((s) => {
      s.hevctTranscode = checked;
    });
  });
}

const dirInput = document.getElementById('download-dir');
if (dirInput) {
  dirInput.addEventListener('blur', async (e) => {
    const value = String(/** @type {HTMLInputElement} */ (e.target).value || '').trim();
    const current = String((state.settings || DEFAULT_SETTINGS).downloadDir || '');
    if (value === current) return; // 没变化不提交
    if (!value) {
      // 空目录不提交，直接回滚显示
      showToast('保存目录不能为空', 'error');
      renderSettings();
      return;
    }
    await applySettings((s) => {
      s.downloadDir = value;
    });
  });
  dirInput.addEventListener('keydown', (e) => {
    if (/** @type {KeyboardEvent} */ (e).key === 'Enter') /** @type {HTMLElement} */ (e.target).blur();
  });
}

/* 外部工具路径：失焦 / 回车提交；留空表示自动检测 */
for (const [id, key] of [
  ['re-path', 'rePath'],
  ['ytdlp-path', 'ytdlpPath'],
  ['ffmpeg-path', 'ffmpegPath'],
  ['tdl-path', 'tdlPath'],
]) {
  const el = /** @type {?HTMLInputElement} */ (document.getElementById(id));
  if (!el) continue;
  el.addEventListener('blur', async (e) => {
    const value = String(/** @type {HTMLInputElement} */ (e.target).value || '').trim();
    const current = String((state.settings || DEFAULT_SETTINGS)[key] || '');
    if (value === current) return;
    await applySettings((s) => { s[key] = value; });
  });
  el.addEventListener('keydown', (e) => {
    if (/** @type {KeyboardEvent} */ (e).key === 'Enter') /** @type {HTMLElement} */ (e.target).blur();
  });
}

/* Telegram 登录：tdl 必须在真终端里扫码，后端会弹一个控制台窗口 */
const loginTdlBtn = /** @type {?HTMLButtonElement} */ (document.getElementById('login-tdl'));
if (loginTdlBtn) {
  loginTdlBtn.addEventListener('click', async () => {
    if (loginTdlBtn.disabled) return;
    loginTdlBtn.disabled = true;
    try {
      await AppApi.LoginTelegram();
      showToast('已打开登录窗口，扫码后即可下载', 'ok');
    } catch (err) {
      showToast('打开登录窗口失败：' + errMsg(err), 'error');
    } finally {
      loginTdlBtn.disabled = false;
    }
  });
}

/* 外部工具：文件夹图标按钮 → 系统目录选择框 */
for (const [btnId, key, label] of [
  ['pick-re', 'rePath', 'RE'],
  ['pick-ytdlp', 'ytdlpPath', 'yt-dlp'],
  ['pick-ffmpeg', 'ffmpegPath', 'ffmpeg'],
  ['pick-tdl', 'tdlPath', 'tdl'],
]) {
  const btn = /** @type {?HTMLButtonElement} */ (document.getElementById(btnId));
  if (!btn) continue;
  btn.addEventListener('click', async () => {
    if (btn.disabled) return;
    btn.disabled = true;
    try {
      const dir = await PickToolFolder();
      const picked = String(dir || '').trim();
      if (!picked) return; // 用户取消
      await applySettings((s) => { s[key] = picked; });
      showToast('已设置 ' + label + ' 目录', 'ok');
    } catch (err) {
      showToast('选择文件夹失败：' + errMsg(err), 'error');
    } finally {
      btn.disabled = false;
    }
  });
}

/* 缓存信息 + 清理缓存 */
function formatBytes(n) {
  const v = Number(n) || 0;
  if (v < 1024) return v + ' B';
  if (v < 1024 * 1024) return (v / 1024).toFixed(1) + ' KB';
  if (v < 1024 * 1024 * 1024) return (v / 1024 / 1024).toFixed(1) + ' MB';
  return (v / 1024 / 1024 / 1024).toFixed(2) + ' GB';
}

/**
 * 读取缓存占用。
 * 后端统计大目录时可能来不及，会返回 pending:true（数字偏旧），
 * 这里自动隔一会儿重试，最多 3 次，期间显示"正在统计"而不是假数字。
 * @param {number} [attempt]
 */
async function refreshCacheInfo(attempt = 0) {
  const el = document.getElementById('cache-info');
  if (!el || typeof CacheInfo !== 'function') return;
  try {
    const info = await CacheInfo();
    if (info && typeof info === 'object') {
      el.title = String(info.path || '');
      const pending = info.pending === true;
      if (pending && !info.bytes) {
        el.textContent = '正在统计缓存…';
      } else {
        el.textContent = '当前占用 ' + formatBytes(info.bytes) + ' · 清理不会删除成品与设置';
      }
      if (pending && attempt < 3) {
        window.setTimeout(() => void refreshCacheInfo(attempt + 1), 600);
      }
    }
  } catch (err) {
    console.error('[cache] 读取缓存信息失败', err);
  }
}

const clearCacheBtn = /** @type {?HTMLButtonElement} */ (document.getElementById('clear-cache'));
if (clearCacheBtn) {
  clearCacheBtn.addEventListener('click', async () => {
    if (clearCacheBtn.disabled) return;
    clearCacheBtn.disabled = true;
    try {
      const freed = await ClearCache();
      showToast('已清理缓存，释放 ' + formatBytes(freed), 'ok');
      await refreshCacheInfo();
    } catch (err) {
      showToast('清理缓存失败：' + errMsg(err), 'error');
    } finally {
      clearCacheBtn.disabled = false;
    }
  });
}

const threadInput = /** @type {?HTMLInputElement} */ (document.getElementById('thread-count'));
if (threadInput) {
  threadInput.addEventListener('blur', async (e) => {
    const raw = String(/** @type {HTMLInputElement} */ (e.target).value || '').trim();
    const parsed = Number.parseInt(raw, 10);
    if (!Number.isFinite(parsed)) {
      showToast('下载线程数需为 1～64 的整数', 'error');
      renderSettings();
      return;
    }
    const value = clampThreadCount(parsed);
    threadInput.value = String(value);
    const current = clampThreadCount((state.settings || DEFAULT_SETTINGS).threadCount);
    if (value === current) return;
    await applySettings((s) => {
      s.threadCount = value;
    });
  });
  threadInput.addEventListener('keydown', (e) => {
    if (/** @type {KeyboardEvent} */ (e).key === 'Enter') /** @type {HTMLElement} */ (e.target).blur();
  });
}

const pickDirBtn = /** @type {?HTMLButtonElement} */ (document.getElementById('pick-dir'));
if (pickDirBtn) {
  let picking = false;
  pickDirBtn.addEventListener('click', async () => {
    if (picking) return;
    picking = true;
    pickDirBtn.disabled = true;
    try {
      const dir = await PickFolder();
      const picked = String(dir || '').trim();
      if (!picked) return; // 用户取消
      await applySettings((s) => {
        s.downloadDir = picked;
      });
    } catch (err) {
      console.error('[settings] 选择文件夹失败', err);
      showToast('选择文件夹失败：' + errMsg(err), 'error');
    } finally {
      picking = false;
      pickDirBtn.disabled = false;
      renderSettings();
    }
  });
}

/* =========================================================================
 * 迷你模式
 * ========================================================================= */
let isMini = false;
let miniSelectedId = '';
let lastMiniIdle = null;
let miniToggling = false;

/** 统一切换迷你/主界面，防止两个入口各自处理造成状态不一致。 */
async function toggleMiniMode() {
  if (miniToggling) return;
  miniToggling = true;
  try {
    await ToggleMini();
  } catch (err) {
    console.error('[window] 切换迷你模式失败', err);
    showToast('切换迷你模式失败：' + errMsg(err), 'error');
  } finally {
    miniToggling = false;
  }
}

const toggleMiniBtn = /** @type {?HTMLButtonElement} */ (document.getElementById('toggle-mini'));
if (toggleMiniBtn) {
  toggleMiniBtn.addEventListener('click', () => {
    toggleMiniBtn.disabled = true;
    void toggleMiniMode().finally(() => {
      toggleMiniBtn.disabled = false;
    });
  });
}

const miniRestoreBtn = document.getElementById('mini-restore');
if (miniRestoreBtn) {
  miniRestoreBtn.addEventListener('click', () => void toggleMiniMode());
}

/* 迷你模式的置顶图钉：点击切换，图标高亮表示当前置顶 */
const miniPinBtn = /** @type {?HTMLButtonElement} */ (document.getElementById('mini-pin'));
function setPinUI(on) {
  if (!miniPinBtn) return;
  miniPinBtn.classList.toggle('is-on', !!on);
  miniPinBtn.setAttribute('aria-pressed', on ? 'true' : 'false');
  miniPinBtn.title = on ? '取消置顶' : '置顶窗口';
}
if (miniPinBtn) {
  miniPinBtn.addEventListener('click', async () => {
    miniPinBtn.disabled = true;
    try {
      setPinUI(await ToggleAlwaysOnTop());
    } catch (err) {
      console.error('[pin] 切换置顶失败', err);
    } finally {
      miniPinBtn.disabled = false;
    }
  });
  void AlwaysOnTop().then(setPinUI).catch(() => {});
}

const miniPrevBtn = /** @type {?HTMLButtonElement} */ (document.getElementById('mini-prev'));
const miniNextBtn = /** @type {?HTMLButtonElement} */ (document.getElementById('mini-next'));

/**
 * mini 中可切换的任务池：优先未完成任务，全部结束则展示最近记录。
 * 后端任务顺序为旧 -> 新，这里反转成新 -> 旧。
 * @returns {Array<object>}
 */
function miniTaskPool() {
  const unfinished = state.tasks.filter((t) => taskGroup(t) === 'active');
  const pool = unfinished.length ? unfinished : state.tasks;
  return pool.slice().reverse();
}

/** @param {number} delta */
function cycleMiniTask(delta) {
  const pool = miniTaskPool();
  if (pool.length <= 1) return;
  const current = pool.findIndex((t) => t.id === miniSelectedId);
  const next = (Math.max(0, current) + delta + pool.length) % pool.length;
  miniSelectedId = pool[next].id;
  renderMini();
}

if (miniPrevBtn) miniPrevBtn.addEventListener('click', () => cycleMiniTask(-1));
if (miniNextBtn) miniNextBtn.addEventListener('click', () => cycleMiniTask(1));

EventsOn('mini-changed', (mini) => {
  isMini = !!mini;
  lastMiniIdle = null;
  const miniView = document.getElementById('mini-view');
  const pageTasks = document.getElementById('page-tasks');
  const pageSettings = document.getElementById('page-settings');
  const topbar = document.querySelector('.topbar');
  if (miniView) miniView.classList.toggle('hide', !isMini);
  if (pageTasks) pageTasks.classList.toggle('hide', isMini);
  if (pageSettings) pageSettings.classList.toggle('hide', isMini);
  if (topbar) topbar.classList.toggle('hide', isMini);
  if (isMini) renderMini();
});

/**
 * 挑一个最适合在迷你模式展示的任务：保留用户手动切换的选择；
 * 若选择已消失，则优先最新的进行中任务，最后回退到最近任务。
 * @returns {?object}
 */
function pickMiniTask() {
  const pool = miniTaskPool();
  if (!pool.length) {
    miniSelectedId = '';
    return null;
  }
  let selected = pool.find((t) => t.id === miniSelectedId);
  if (!selected) {
    // 首次进入 mini 时优先展示真正在动的任务，避免恰好选中已暂停的旧任务。
    selected =
      pool.find((t) => ['running', 'transcoding', 'pausing', 'queued', 'deleting'].includes(effectiveStatus(t))) ||
      pool[0];
    miniSelectedId = selected.id;
  }
  return selected;
}

/** 根据任务数量切换 mini 窗口高度，空闲时收成更小卡片。 */
function ensureMiniWindowSize(idle) {
  if (!isMini || lastMiniIdle === idle) return;
  lastMiniIdle = idle;
  try {
    if (idle) WindowSetSize(300, 84);
    else WindowSetSize(400, 150);
  } catch (err) {
    console.error('[window] 调整迷你窗口尺寸失败', err);
  }
}

/** 渲染迷你模式卡片 */
function renderMini() {
  const view = document.getElementById('mini-view');
  const titleEl = document.getElementById('mini-title');
  const barEl = document.getElementById('mini-bar');
  const speedEl = document.getElementById('mini-speed');
  const segEl = document.getElementById('mini-segments');
  const pctEl = document.getElementById('mini-pct');
  const statusEl = document.getElementById('mini-status-label');
  const dotEl = document.getElementById('mini-state-dot');
  const queueEl = document.getElementById('mini-queue');
  const indexEl = document.getElementById('mini-index');
  const pauseBtn = /** @type {?HTMLButtonElement} */ (document.getElementById('mini-pause'));
  if (!titleEl || !barEl || !speedEl || !segEl || !pctEl || !statusEl || !dotEl || !queueEl || !indexEl) return;

  const pool = miniTaskPool();
  const t = pickMiniTask();
  const idle = !t;
  if (view) view.classList.toggle('is-idle', idle);
  ensureMiniWindowSize(idle);

  if (miniPrevBtn) miniPrevBtn.disabled = pool.length <= 1;
  if (miniNextBtn) miniNextBtn.disabled = pool.length <= 1;

  if (!t) {
    statusEl.textContent = '待机';
    dotEl.className = 'mini-state-dot idle';
    queueEl.textContent = '没有任务';
    titleEl.textContent = 'HLSink';
    titleEl.title = '';
    indexEl.textContent = '';
    barEl.style.width = '0%';
    barEl.className = 'mini-track-fill';
    speedEl.textContent = '等待新任务';
    segEl.textContent = '';
    pctEl.textContent = '';
    if (pauseBtn) pauseBtn.style.display = 'none';
    return;
  }

  const status = effectiveStatus(t);
  const label = statusText[safeStatus(status)] || safeStatus(status);
  const name = String(t.title || t.filename || '任务');
  const idx = Math.max(0, pool.findIndex((x) => x.id === t.id));
  const activeCount = state.tasks.filter((x) => taskGroup(x) === 'active').length;
  const queued = state.tasks.filter((x) => effectiveStatus(x) === 'queued').length;
  const info = progressInfo(t.progress);

  statusEl.textContent = label;
  let dotClass = 'mini-state-dot';
  if (status === 'running' || status === 'transcoding' || status === 'pausing' || status === 'deleting') {
    dotClass += ' pulse';
  } else if (status === 'paused') {
    dotClass += ' paused';
  } else if (status === 'done') {
    dotClass += ' done';
  } else if (status === 'error' || status === 'cleanup_failed') {
    dotClass += ' error';
  } else {
    dotClass += ' idle';
  }
  dotEl.className = dotClass;
  queueEl.textContent = activeCount
    ? `${activeCount} 个任务${queued ? ` · ${queued} 个排队` : ''}`
    : '最近任务';

  titleEl.textContent = name;
  titleEl.title = t.url ? name + '\n' + String(t.url) + '\n双击返回主界面' : '双击返回主界面';
  indexEl.textContent = `${idx + 1} / ${pool.length}`;
  barEl.style.width = info.width;
  let fillClass = 'mini-track-fill';
  if (status === 'done') fillClass += ' done';
  else if (status === 'paused') fillClass += ' paused';
  else if (status === 'error' || status === 'cleanup_failed') fillClass += ' failed';
  barEl.className = fillClass;

  speedEl.textContent = status === 'running' && t.speed ? String(t.speed) : label;
  segEl.textContent = t.segments ? String(t.segments) + ' 片' : '';
  pctEl.textContent = status === 'done' ? '100%' : info.text;

  if (pauseBtn) {
    const canPause = status === 'running' || status === 'queued';
    const canResume = status === 'paused';
    pauseBtn.style.display = canPause || canResume ? '' : 'none';
    pauseBtn.textContent = canResume ? '继续' : '暂停';
    pauseBtn.disabled = state.busy.has(t.id) || !(canPause || canResume);
    pauseBtn.onclick = () => {
      const current = state.tasks.find((x) => x.id === t.id);
      if (!current) return;
      void handleTaskAction(current, canResume ? 'resume' : 'pause');
    };
  }
}

/** 迷你模式可见时才重绘 */
function renderMiniIfMini() {
  if (isMini) renderMini();
}

/** 双击 mini 标题返回主界面。 */
const miniTitleEl = document.getElementById('mini-title');
if (miniTitleEl) {
  miniTitleEl.addEventListener('dblclick', () => void toggleMiniMode());
}

/** 统一刷新入口：列表 + 迷你卡片 */
function refresh() {
  renderTasks();
  renderMiniIfMini();
}

/* =========================================================================
 * 服务状态（扩展接口 http://127.0.0.1:18752）
 * ========================================================================= */

/** @type {?HTMLElement} */
let serverTextEl = null;
/** @type {?HTMLElement} */
let serverSettingsEl = null;

/**
 * 更新服务状态显示。
 * @param {boolean} online
 * @param {string} [note]
 */
function setServerStatus(online, note) {
  if (!serverTextEl) serverTextEl = document.getElementById('server-text');
  if (!serverSettingsEl) serverSettingsEl = document.getElementById('server-status-text');
  const text = note || (online ? '运行中' : '离线');
  for (const node of [serverTextEl, serverSettingsEl]) {
    if (!node) continue;
    node.textContent = text;
    node.classList.toggle('ok', !!online);
    node.classList.toggle('bad', !online);
    node.title = online ? '扩展接口已就绪' : '扩展接口未响应，可能是软件刚启动或端口被占用';
  }
}

/**
 * 带超时的 fetch。
 * @param {string} url
 * @param {RequestInit} init
 * @param {number} ms
 * @returns {Promise<Response>}
 */
async function fetchWithTimeout(url, init, ms) {
  const ctrl = new AbortController();
  const timer = window.setTimeout(() => ctrl.abort(), ms);
  try {
    return await fetch(url, { ...init, signal: ctrl.signal });
  } finally {
    window.clearTimeout(timer);
  }
}

/**
 * 可选绑定探测：后端若新增 ServerStatus() bool，用它最准。
 * 用动态 import + 存在性判断，后端没加时安全返回 null，不会让整个模块加载失败。
 * @returns {Promise<?boolean>}
 */
let appApiModule = null;
async function callOptionalServerStatus() {
  if (appApiModule === false) return null;
  if (appApiModule === null) {
    try {
      appApiModule = AppApi;
    } catch (err) {
      console.error('[server] 动态加载绑定失败', err);
      appApiModule = false;
      return null;
    }
  }
  const fn = appApiModule && /** @type {any} */ (appApiModule).ServerStatus;
  if (typeof fn !== 'function') return null;
  try {
    return !!/** @type {any} */ (await fn());
  } catch (err) {
    console.error('[server] ServerStatus 调用失败', err);
    return null;
  }
}

/**
 * 探测服务是否在线。
 * 1) 优先用后端 ServerStatus()（若存在）
 * 2) 其次带 CORS 读 /ping，能校验 app === "HLSink"
 * 3) 降级 no-cors 探活：只判断端口是否在监听（页面 Origin 是 wails 本地服务，
 *    后端 /ping 只放行扩展 Origin，正常 CORS 读会被拦，所以需要降级探测）
 * @returns {Promise<boolean>}
 */
async function probeServer() {
  const bound = await callOptionalServerStatus();
  if (bound !== null) {
    setServerStatus(bound, bound ? '运行中' : '离线');
    return bound;
  }

  try {
    const res = await fetchWithTimeout(PING_URL, { cache: 'no-store', credentials: 'omit' }, PING_TIMEOUT_MS);
    if (res.ok) {
      const data = await res.json();
      if (data && data.app === 'HLSink') {
        setServerStatus(true, '运行中');
        return true;
      }
      setServerStatus(false, '端口被其他程序占用');
      return false;
    }
    // 有 HTTP 响应（如来源受限的 403）说明端口在监听
    setServerStatus(true, '运行中');
    return true;
  } catch (_err) {
    // CORS 被拦或网络失败，走降级探活
  }

  try {
    await fetchWithTimeout(PING_URL, { mode: 'no-cors', cache: 'no-store', credentials: 'omit' }, PING_TIMEOUT_MS);
    setServerStatus(true, '运行中');
    return true;
  } catch (err) {
    console.error('[server] /ping 探测失败', err);
    setServerStatus(false, '离线');
    return false;
  }
}

/* =========================================================================
 * 后端事件
 * ========================================================================= */
EventsOn('task-added', (t) => {
  if (!t || !t.id) return;
  const i = state.tasks.findIndex((x) => x.id === t.id);
  if (i >= 0) state.tasks[i] = t;
  else state.tasks.push(t);
  state.optStatus.delete(t.id);
  refresh();
});

EventsOn('task-updated', (t) => {
  if (!t || !t.id) return;
  const i = state.tasks.findIndex((x) => x.id === t.id);
  const prevStatus = i >= 0 ? safeStatus(state.tasks[i].status) : '';
  if (i >= 0) state.tasks[i] = t;
  else state.tasks.push(t);
  // 后端快照是权威的，清掉乐观状态（busy 由发起的请求自己释放，避免提前放开按钮重复点击）
  state.optStatus.delete(t.id);

  const nextStatus = safeStatus(t.status);
  if (nextStatus === 'cleanup_failed' && prevStatus !== 'cleanup_failed') {
    const name = String(t.title || t.filename || '任务');
    showToast('「' + name + '」的文件还被占用，正在后台清理，稍后自动消失', 'info');
  }
  if (nextStatus !== 'error' && nextStatus !== 'cleanup_failed') state.actionError.delete(t.id);
  refresh();
});

EventsOn('task-removed', (id) => {
  const key = String(id || '');
  state.tasks = state.tasks.filter((x) => x.id !== key);
  state.busy.delete(key);
  state.optStatus.delete(key);
  state.actionError.delete(key);
  refresh();
});

EventsOn('settings-changed', (s) => {
  if (!s || typeof s !== 'object') return;
  state.settings = s;
  renderSettings();
  renderMiniIfMini();
});

EventsOn('server-ready', () => {
  setServerStatus(true, '运行中');
});

/* =========================================================================
 * 启动
 * ========================================================================= */
async function init() {
  try {
    const list = await ListTasks();
    state.tasks = Array.isArray(list) ? list : [];
  } catch (err) {
    console.error('[init] 加载任务列表失败', err);
    state.tasks = [];
    showToast('加载任务列表失败：' + errMsg(err), 'error');
  }

  try {
    const s = await GetSettings();
    state.settings = s && typeof s === 'object' ? s : { ...DEFAULT_SETTINGS };
  } catch (err) {
    console.error('[init] 加载设置失败', err);
    state.settings = { ...DEFAULT_SETTINGS };
    showToast('加载设置失败：' + errMsg(err), 'error');
  }

  renderSettings();
  refresh();
  void refreshCacheInfo();
  void refreshToolStates();

  // 页面初始可能错过 server-ready 事件，主动探测一次，之后定时刷新
  void probeServer();
  // 后端 HTTP 服务可能比页面晚几十毫秒就绪，补一次快速重试，避免误显示离线
  window.setTimeout(() => void probeServer(), 900);
  window.setInterval(() => void probeServer(), SERVER_PROBE_INTERVAL_MS);
}

await init();
})();
