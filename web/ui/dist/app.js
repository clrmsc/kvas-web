// Интерфейс управления Квасом. Без сборщика и внешних библиотек:
// файл вшивается в бинарник и должен открываться на роутере как есть.

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

/* ---------------- Работа с API ---------------- */

class AuthError extends Error {}

async function api(path, { method = 'GET', body } = {}) {
  const res = await fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
    credentials: 'same-origin',
  });
  if (res.status === 401) throw new AuthError('требуется вход');

  const text = await res.text();
  let data = {};
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      throw new Error('сервис вернул неожиданный ответ');
    }
  }
  if (!res.ok) throw new Error(data.error || `ошибка ${res.status}`);
  return data;
}

// Разбор потока server-sent events: операции импорта и обслуживания
// присылают ход выполнения построчно.
async function stream(path, { body, onEvent }) {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body ?? {}),
    credentials: 'same-origin',
  });
  if (res.status === 401) throw new AuthError('требуется вход');
  if (!res.ok) {
    const text = await res.text();
    let msg = `ошибка ${res.status}`;
    try { msg = JSON.parse(text).error || msg; } catch { /* пустой ответ */ }
    throw new Error(msg);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const chunks = buffer.split('\n\n');
    buffer = chunks.pop();
    for (const chunk of chunks) {
      let event = 'message';
      let data = '';
      for (const line of chunk.split('\n')) {
        if (line.startsWith('event: ')) event = line.slice(7).trim();
        else if (line.startsWith('data: ')) data += line.slice(6);
      }
      if (!data) continue;
      try { onEvent(event, JSON.parse(data)); } catch { /* пропускаем битый кадр */ }
    }
  }
}

/* ---------------- Уведомления ---------------- */

function toast(message, kind = 'ok') {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.textContent = message;
  $('#toasts').append(el);
  setTimeout(() => el.remove(), kind === 'err' ? 6000 : 3200);
}

// Общая точка обработки ошибок: истёкшая сессия возвращает на экран входа.
function handle(err) {
  if (err instanceof AuthError) {
    showLogin();
    return;
  }
  toast(err.message, 'err');
}

/* ---------------- Вход ---------------- */

let needsSetup = false;

function showLogin() {
  $('#app').classList.add('hidden');
  $('#login').classList.remove('hidden');
  $('#login-pass').focus();
}

function showApp() {
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
  refreshAll();
}

async function checkAuth() {
  const state = await api('/api/auth/state');
  needsSetup = !state.has_password;

  if (needsSetup) {
    $('#login-title').textContent = 'Первый запуск';
    $('#login-hint').textContent =
      `Придумайте пароль администратора — минимум ${state.min_password_len} символов.`;
    $('#login-pass').placeholder = 'Новый пароль';
    $('#login-pass').autocomplete = 'new-password';
    $('#login-repeat-row').classList.remove('hidden');
    $('#login-submit').textContent = 'Сохранить и войти';
  }

  if (state.authenticated) showApp();
  else showLogin();
}

$('#login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const pass = $('#login-pass').value;
  const submit = $('#login-submit');
  submit.disabled = true;
  try {
    if (needsSetup) {
      if (pass !== $('#login-pass2').value) throw new Error('пароли не совпадают');
      await api('/api/auth/setup', { method: 'POST', body: { password: pass } });
    } else {
      await api('/api/auth/login', { method: 'POST', body: { password: pass } });
    }
    $('#login-form').reset();
    needsSetup = false;
    showApp();
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    submit.disabled = false;
  }
});

$('#btn-logout').addEventListener('click', async () => {
  try {
    await api('/api/auth/logout', { method: 'POST' });
  } catch { /* выходим в любом случае */ }
  location.reload();
});

/* ---------------- Вкладки ---------------- */

const loaders = {};

function selectTab(name) {
  $$('nav.tabs button').forEach((b) =>
    b.setAttribute('aria-selected', String(b.dataset.tab === name)));
  $$('main section').forEach((s) =>
    s.classList.toggle('hidden', s.dataset.panel !== name));
  loaders[name]?.().catch(handle);
}

$$('nav.tabs button').forEach((b) =>
  b.addEventListener('click', () => selectTab(b.dataset.tab)));

/* ---------------- Обзор ---------------- */

const MODE_NAMES = { vless: 'VLESS', hysteria: 'Hysteria', none: 'не настроен' };

loaders.overview = async () => {
  const s = await api('/api/status');

  $('#hdr-version').textContent = s.version ? `v${s.version}` : '';

  const active = s.mode === 'vless' ? s.vless : s.mode === 'hysteria' ? s.hysteria : null;
  const tunnelUp = Boolean(active?.running && active?.tunnel);
  const hdr = $('#hdr-tunnel');
  $('.dot', hdr).className = `dot ${s.mode === 'none' ? '' : tunnelUp ? 'ok' : 'err'}`;
  $('.text', hdr).textContent = s.mode === 'none'
    ? 'туннель не настроен'
    : tunnelUp ? 'туннель работает' : 'туннель недоступен';

  const svcState = (svc) => {
    if (!svc.running) return ['err', 'остановлен'];
    if (!svc.tunnel) return ['warn', 'запущен, порт закрыт'];
    return ['ok', 'работает'];
  };
  const [vlessDot, vlessText] = svcState(s.vless);
  const [hystDot, hystText] = svcState(s.hysteria);

  $('#status-grid').innerHTML = [
    stat('Режим', MODE_NAMES[s.mode] ?? s.mode),
    stat('Доменов в списке', s.hosts),
    stat('Заквасок', s.tags),
    stat('Блокировка рекламы', s.adblock ? 'включена' : 'выключена'),
    statBadge('VLESS', vlessDot, vlessText),
    statBadge('Hysteria', hystDot, hystText),
    stat('Интерфейс', s.interface || '—'),
    stat('Резервный канал', s.failover === 'on' ? 'включён' : 'ручной режим'),
    s.subscription_server ? stat('Сервер из подписки', s.subscription_server) : '',
  ].join('');
};

const stat = (label, value) =>
  `<div class="stat"><div class="label">${esc(label)}</div><div class="value">${esc(String(value))}</div></div>`;

const statBadge = (label, dot, text) =>
  `<div class="stat"><div class="label">${esc(label)}</div>
   <div class="value" style="font-size:15px"><span class="badge"><span class="dot ${dot}"></span>${esc(text)}</span></div></div>`;

// Обслуживание: init и update отдают вывод потоком.
$$('[data-service]').forEach((btn) => btn.addEventListener('click', async () => {
  const log = $('#service-log');
  log.classList.remove('hidden');
  log.textContent = '';
  $$('[data-service]').forEach((b) => (b.disabled = true));
  try {
    await stream(`/api/service/${btn.dataset.service}`, {
      onEvent: (event, data) => {
        if (event === 'line') appendLog(log, data.line);
        if (event === 'error') appendLog(log, `ОШИБКА: ${data.error}`);
        if (event === 'done') { appendLog(log, 'Готово.'); toast('Операция завершена'); }
      },
    });
    loaders.overview().catch(handle);
  } catch (err) {
    handle(err);
  } finally {
    $$('[data-service]').forEach((b) => (b.disabled = false));
  }
}));

$('#btn-backup').addEventListener('click', async () => {
  try {
    const r = await api('/api/service/backup', { method: 'POST' });
    toast(r.msg);
    if (r.output) {
      const log = $('#service-log');
      log.classList.remove('hidden');
      log.textContent = r.output;
    }
  } catch (err) { handle(err); }
});

function appendLog(el, line) {
  el.textContent += (el.textContent ? '\n' : '') + line;
  el.scrollTop = el.scrollHeight;
}

/* ---------------- Домены ---------------- */

let allHosts = [];

loaders.hosts = async () => {
  const r = await api('/api/hosts');
  allHosts = r.hosts;
  $('#hosts-count').textContent = `(${allHosts.length})`;
  renderHosts();
};

function renderHosts() {
  const filter = $('#host-filter').value.trim().toLowerCase();
  const items = filter ? allHosts.filter((h) => h.includes(filter)) : allHosts;
  const list = $('#hosts-list');

  if (!items.length) {
    list.innerHTML = `<div class="empty">${allHosts.length ? 'Ничего не найдено' : 'Список пуст'}</div>`;
    return;
  }
  list.innerHTML = items.map((h) => `
    <div class="item">
      <span class="name">${esc(h)}</span>
      <button class="btn small danger" data-del-host="${esc(h)}">Удалить</button>
    </div>`).join('');
}

$('#host-filter').addEventListener('input', renderHosts);

$('#host-add-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const input = $('#host-input');
  try {
    const r = await api('/api/hosts', { method: 'POST', body: { domain: input.value } });
    toast(r.msg);
    input.value = '';
    await loaders.hosts();
  } catch (err) { handle(err); }
});

$('#hosts-list').addEventListener('click', async (e) => {
  const domain = e.target.dataset?.delHost;
  if (!domain) return;
  if (!confirm(`Удалить ${domain} из списка?`)) return;
  e.target.disabled = true;
  try {
    const r = await api(`/api/hosts/${encodeURIComponent(domain)}`, { method: 'DELETE' });
    toast(r.msg);
    await loaders.hosts();
  } catch (err) { handle(err); e.target.disabled = false; }
});

$('#btn-import').addEventListener('click', async () => {
  const text = $('#import-text').value.trim();
  if (!text) { toast('Список пуст', 'err'); return; }

  const log = $('#import-log');
  log.classList.remove('hidden');
  log.textContent = '';
  $('#btn-import').disabled = true;
  try {
    await stream('/api/hosts/import', {
      body: { domains: text },
      onEvent: (event, data) => {
        if (event === 'start') {
          appendLog(log, `Импортируем ${data.total} доменов` +
            (data.skipped ? `, пропущено некорректных: ${data.skipped}` : ''));
        }
        if (event === 'line') appendLog(log, data.line);
        if (event === 'error') appendLog(log, `ОШИБКА: ${data.error}`);
        if (event === 'done') {
          appendLog(log, `Готово: добавлено ${data.imported}`);
          toast('Импорт завершён');
          $('#import-text').value = '';
          loaders.hosts().catch(handle);
        }
      },
    });
  } catch (err) {
    handle(err);
  } finally {
    $('#btn-import').disabled = false;
  }
});

/* ---------------- Закваски ---------------- */

loaders.tags = async () => {
  const r = await api('/api/tags');
  const list = $('#tags-list');
  if (!r.tags.length) {
    list.innerHTML = '<div class="empty">Заквасок нет</div>';
    return;
  }
  list.innerHTML = r.tags.map((t) => {
    const inList = t.domains.filter((d) => d.in_list).length;
    return `
      <div class="item">
        <span class="name">${esc(t.name)}
          <div class="meta">${inList} из ${t.domains.length} доменов в списке</div>
        </span>
        <button class="btn small ${t.enabled ? 'danger' : 'primary'}"
                data-tag="${esc(t.name)}" data-action="${t.enabled ? 'disable' : 'enable'}">
          ${t.enabled ? 'Выключить' : 'Включить'}
        </button>
      </div>`;
  }).join('');
};

$('#tags-list').addEventListener('click', async (e) => {
  const { tag, action } = e.target.dataset ?? {};
  if (!tag) return;
  e.target.disabled = true;
  e.target.textContent = 'Применяем…';
  try {
    const r = await api(`/api/tags/${encodeURIComponent(tag)}/${action}`, { method: 'POST' });
    toast(r.msg);
    await loaders.tags();
  } catch (err) { handle(err); await loaders.tags(); }
});


/* ---------------- Подписка ---------------- */

let subState = null;

loaders.subscription = async () => {
  subState = await api('/api/subscription');
  renderSubscription();
};

function renderSubscription() {
  const s = subState;

  $('#sub-url-current').textContent = s.configured
    ? `Текущая ссылка: ${s.url_masked}`
    : 'Ссылка ещё не задана';

  $('#sub-enabled').checked = s.enabled;
  $('#sub-autoapply').checked = s.auto_apply;
  $('#sub-time').value = normalizeTime(s.check_time);
  $('#sub-topn').value = String(s.speed_top_n);

  $('#sub-status').innerHTML = [
    stat('Активный сервер', s.active_name || 'не выбран'),
    stat('Переключён', s.applied_at || '—'),
    stat('Последняя проверка', s.last_check || 'ещё не было'),
    stat('Следующая проверка', s.next_check || (s.enabled ? '—' : 'выключена')),
  ].join('');

  if (s.last_error) {
    $('#sub-status').insertAdjacentHTML('beforeend',
      `<div class="stat" style="grid-column:1/-1">
         <div class="label">Последняя ошибка</div>
         <div class="value" style="font-size:14px">${esc(s.last_error)}</div>
       </div>`);
  }

  renderSubResults(s.results ?? []);
}

// Время из состояния приходит как «4:30», а полю нужно «04:30».
function normalizeTime(value) {
  const [h = '4', m = '30'] = String(value || '').split(':');
  return `${h.padStart(2, '0')}:${m.padStart(2, '0')}`;
}

function renderSubResults(results) {
  const list = $('#sub-results');
  if (!results.length) {
    list.innerHTML = '<div class="empty">Проверок ещё не было</div>';
    return;
  }
  list.innerHTML = results.map((r) => {
    const active = r.key === subState?.active_key;
    let metrics;
    if (r.error) {
      metrics = `<span style="color:var(--err)">${esc(shortError(r.error))}</span>`;
    } else {
      const speed = r.speed_mbps
        ? `${r.speed_mbps.toFixed(1)} Мбит/с${r.speed_stale ? ' (прошлый замер)' : ''}`
        : r.speed_error
          ? `скорость не измерена: ${esc(shortError(r.speed_error))}`
          : 'скорость ещё не мерили';
      metrics = `${Math.round(r.latency_ms)} мс · ${speed}`;
    }

    return `
      <div class="item">
        <span class="name">${esc(r.name)}
          <div class="meta">${esc(r.address)}:${r.port} — ${metrics}</div>
        </span>
        ${active
          ? '<span class="badge"><span class="dot ok"></span>активен</span>'
          : `<button class="btn small" data-apply="${esc(r.key)}"${r.error ? ' disabled' : ''}>Применить</button>`}
      </div>`;
  }).join('');
}

// Ошибки от xray и сети бывают многострочными — в списке нужна одна строка.
function shortError(text) {
  const line = String(text).split('\n')[0];
  return line.length > 90 ? `${line.slice(0, 90)}…` : line;
}

$('#sub-url-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const url = $('#sub-url').value.trim();
  if (!url) { toast('Вставьте ссылку на подписку', 'err'); return; }
  try {
    subState = await api('/api/subscription', { method: 'POST', body: { url } });
    $('#sub-url').value = '';
    renderSubscription();
    toast('Ссылка сохранена');
  } catch (err) { handle(err); }
});

$('#sub-save-schedule').addEventListener('click', async () => {
  try {
    subState = await api('/api/subscription', {
      method: 'POST',
      body: {
        enabled: $('#sub-enabled').checked,
        auto_apply: $('#sub-autoapply').checked,
        check_time: $('#sub-time').value || '04:30',
        speed_top_n: Number($('#sub-topn').value),
      },
    });
    renderSubscription();
    toast('Расписание сохранено');
  } catch (err) { handle(err); }
});

$('#sub-check').addEventListener('click', async () => {
  const btn = $('#sub-check');
  const progress = $('#sub-progress');
  const seen = new Map();

  btn.disabled = true;
  progress.textContent = 'Проверяем…';
  try {
    await stream('/api/subscription/check', {
      onEvent: (event, data) => {
        if (event === 'result') {
          // Один сервер приходит дважды: после замера задержки и скорости.
          seen.set(data.key, data);
          progress.textContent = `Проверено серверов: ${seen.size}`;
          renderSubResults([...seen.values()]);
        }
        if (event === 'error') {
          progress.textContent = '';
          toast(data.error, 'err');
        }
        if (event === 'done') {
          subState = data.state;
          progress.textContent = '';
          renderSubscription();
          toast('Проверка завершена');
        }
      },
    });
  } catch (err) {
    progress.textContent = '';
    handle(err);
  } finally {
    btn.disabled = false;
  }
});

$('#sub-results').addEventListener('click', async (e) => {
  const key = e.target.dataset?.apply;
  if (!key) return;
  e.target.disabled = true;
  e.target.textContent = 'Переключаем…';
  try {
    const r = await api('/api/subscription/apply', { method: 'POST', body: { key } });
    subState = r.state;
    renderSubscription();
    toast(r.msg);
  } catch (err) {
    handle(err);
    renderSubResults(subState?.results ?? []);
  }
});

/* ---------------- Устройства ---------------- */


const ROUTE_NAMES = { full: 'весь трафик', list: 'по списку', exclude: 'мимо туннеля' };

loaders.routes = async () => {
  const [routes, devices] = await Promise.all([
    api('/api/routes'),
    api('/api/routes/devices'),
  ]);

  $('#devices').innerHTML = devices.devices
    .map((d) => `<option value="${esc(d.ip)}">${esc(d.name || d.ip)}</option>`).join('');

  const list = $('#routes-list');
  if (!routes.routes.length) {
    list.innerHTML = '<div class="empty">Правил нет — все устройства работают по общему режиму</div>';
    return;
  }
  list.innerHTML = routes.routes.map((r) => `
    <div class="item">
      <span class="name">${esc(r.ip)}
        <div class="meta">${esc(r.device || 'устройство неизвестно')} · ${esc(ROUTE_NAMES[r.type] ?? r.type)}</div>
      </span>
      <button class="btn small danger" data-route-type="${esc(r.type)}" data-route-ip="${esc(r.ip)}">Удалить</button>
    </div>`).join('');
};

$('#route-add-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    const r = await api('/api/routes', {
      method: 'POST',
      body: { type: $('#route-type').value, ip: $('#route-ip').value },
    });
    toast(r.msg);
    $('#route-ip').value = '';
    await loaders.routes();
  } catch (err) { handle(err); }
});

$('#routes-list').addEventListener('click', async (e) => {
  const { routeType, routeIp } = e.target.dataset ?? {};
  if (!routeIp) return;
  e.target.disabled = true;
  try {
    const r = await api(`/api/routes/${routeType}/${encodeURIComponent(routeIp)}`, { method: 'DELETE' });
    toast(r.msg);
    await loaders.routes();
  } catch (err) { handle(err); e.target.disabled = false; }
});

/* ---------------- Реклама ---------------- */

loaders.adblock = async () => {
  const [status, blocked] = await Promise.all([
    api('/api/adblock'),
    api('/api/adblock/blocked'),
  ]);

  $('#adblock-toggle').checked = status.enabled;
  $('#adblock-state').textContent = status.enabled ? 'включена' : 'выключена';

  const list = $('#blocked-list');
  list.innerHTML = blocked.sites.length
    ? blocked.sites.map((d) => `
        <div class="item">
          <span class="name">${esc(d)}</span>
          <button class="btn small danger" data-unblock="${esc(d)}">Разблокировать</button>
        </div>`).join('')
    : '<div class="empty">Заблокированных сайтов нет</div>';
};

$('#adblock-toggle').addEventListener('change', async (e) => {
  const enabled = e.target.checked;
  e.target.disabled = true;
  try {
    const r = await api('/api/adblock', { method: 'POST', body: { enabled } });
    toast(r.msg);
  } catch (err) {
    handle(err);
    e.target.checked = !enabled;
  } finally {
    e.target.disabled = false;
    loaders.adblock().catch(handle);
  }
});

$('#blocked-add-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const input = $('#blocked-input');
  try {
    const r = await api('/api/adblock/blocked', { method: 'POST', body: { domain: input.value } });
    toast(r.msg);
    input.value = '';
    await loaders.adblock();
  } catch (err) { handle(err); }
});

$('#blocked-list').addEventListener('click', async (e) => {
  const domain = e.target.dataset?.unblock;
  if (!domain) return;
  e.target.disabled = true;
  try {
    const r = await api(`/api/adblock/blocked/${encodeURIComponent(domain)}`, { method: 'DELETE' });
    toast(r.msg);
    await loaders.adblock();
  } catch (err) { handle(err); e.target.disabled = false; }
});

/* ---------------- Настройки ---------------- */

$('#password-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    const r = await api('/api/auth/password', {
      method: 'POST',
      body: { current: $('#pass-current').value, new: $('#pass-new').value },
    });
    toast(r.msg);
    // Смена пароля завершает все сессии — возвращаемся ко входу.
    setTimeout(() => location.reload(), 1200);
  } catch (err) { handle(err); }
});

$('#btn-logs').addEventListener('click', () => loaders.settings().catch(handle));

loaders.settings = async () => {
  const r = await api('/api/logs?lines=200');
  $('#logs-view').textContent = r.lines.length ? r.lines.join('\n') : 'Журнал пуст';
  $('#logs-view').scrollTop = $('#logs-view').scrollHeight;
};

/* ---------------- Общее ---------------- */

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function refreshAll() {
  const current = $$('nav.tabs button').find((b) => b.getAttribute('aria-selected') === 'true');
  selectTab(current?.dataset.tab ?? 'overview');
}

checkAuth().catch((err) => {
  if (err instanceof AuthError) showLogin();
  else { toast(err.message, 'err'); showLogin(); }
});
