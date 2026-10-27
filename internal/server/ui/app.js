(() => {
  "use strict";

  const PIN_PATTERN = /^[0-9]{6,12}$/;
  const views = {
    loading: document.getElementById("loadingView"),
    setup: document.getElementById("setupView"),
    login: document.getElementById("loginView"),
    dashboard: document.getElementById("dashboardView"),
  };
  const ui = {
    version: document.getElementById("versionLabel"),
    notice: document.getElementById("notice"),
    setupFormWrap: document.getElementById("setupFormWrap"),
    setupLocalCopy: document.getElementById("setupLocalCopy"),
    setupForm: document.getElementById("setupForm"),
    setupToken: document.getElementById("setupToken"),
    setupTokenWrap: document.getElementById("setupTokenWrap"),
    setupPin: document.getElementById("setupPin"),
    setupPinConfirm: document.getElementById("setupPinConfirm"),
    loginForm: document.getElementById("loginForm"),
    loginPin: document.getElementById("loginPin"),
    statusPanel: document.getElementById("statusPanel"),
    statusTitle: document.getElementById("statusTitle"),
    statusDetail: document.getElementById("statusDetail"),
    toggleButton: document.getElementById("toggleButton"),
    warnings: document.getElementById("dashboardWarnings"),
    gameCount: document.getElementById("gameCount"),
    builtinGamesList: document.getElementById("builtinGamesList"),
    launchGuardBadge: document.getElementById("launchGuardBadge"),
    gamesList: document.getElementById("gamesList"),
    gamesEmpty: document.getElementById("gamesEmpty"),
    gameForm: document.getElementById("gameForm"),
    gameName: document.getElementById("gameName"),
    gameExecutable: document.getElementById("gameExecutable"),
    gamePath: document.getElementById("gamePath"),
    enforcementList: document.getElementById("enforcementList"),
    enforcementEmpty: document.getElementById("enforcementEmpty"),
    eventsList: document.getElementById("eventsList"),
    eventsEmpty: document.getElementById("eventsEmpty"),
    lanUrls: document.getElementById("lanUrls"),
    pinForm: document.getElementById("pinForm"),
    currentPin: document.getElementById("currentPin"),
    newPin: document.getElementById("newPin"),
    newPinConfirm: document.getElementById("newPinConfirm"),
    logoutButton: document.getElementById("logoutButton"),
  };

  let csrfToken = "";
  let currentState = null;
  let bootstrap = null;
  let dashboardPoll = null;
  let eventPoll = null;
  let refreshing = false;
  let setupTokenFromURL = "";

  class APIError extends Error {
    constructor(message, status, code) {
      super(message);
      this.name = "APIError";
      this.status = status;
      this.code = code;
    }
  }

  async function api(path, options = {}) {
    const method = options.method || "GET";
    const headers = Object.assign({ Accept: "application/json" }, options.headers || {});
    const init = {
      method,
      credentials: "same-origin",
      headers,
      cache: "no-store",
    };
    if (options.body !== undefined) {
      headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(options.body);
    }
    if (csrfToken && method !== "GET" && method !== "HEAD") {
      headers["X-CSRF-Token"] = csrfToken;
    }
    let response;
    try {
      response = await fetch(path, init);
    } catch (_error) {
      throw new APIError("Cannot reach Playground Control. Check that the service is running and this device is still on the LAN.", 0, "network");
    }
    const text = await response.text();
    let payload = null;
    if (text) {
      try {
        payload = JSON.parse(text);
      } catch (_error) {
        payload = null;
      }
    }
    if (!response.ok) {
      const message = payload && payload.message ? payload.message : "The request failed.";
      throw new APIError(message, response.status, payload && payload.code ? payload.code : "request_failed");
    }
    return payload;
  }

  function setView(name) {
    Object.entries(views).forEach(([key, element]) => {
      element.hidden = key !== name;
    });
  }

  function showNotice(message, kind = "info") {
    ui.notice.textContent = message;
    ui.notice.className = "notice";
    if (kind === "error") {
      ui.notice.classList.add("error");
    } else if (kind === "success") {
      ui.notice.classList.add("success");
    }
    ui.notice.hidden = false;
    ui.notice.scrollIntoView({ block: "nearest" });
  }

  function clearNotice() {
    ui.notice.hidden = true;
    ui.notice.textContent = "";
  }

  function setBusy(button, busy) {
    if (!button) {
      return;
    }
    button.disabled = busy;
  }

  function friendlyError(error) {
    if (error instanceof APIError) {
      return error.message;
    }
    return "An unexpected error occurred.";
  }

  function startPolling() {
    if (!dashboardPoll) {
      dashboardPoll = window.setInterval(() => {
        if (!document.hidden && !views.dashboard.hidden) {
          refreshState(true);
        }
      }, 2000);
    }
    if (!eventPoll) {
      eventPoll = window.setInterval(() => {
        if (!document.hidden && !views.dashboard.hidden) {
          loadEvents();
        }
      }, 5000);
    }
  }

  function stopDashboard() {
    if (dashboardPoll) {
      window.clearInterval(dashboardPoll);
      dashboardPoll = null;
    }
    if (eventPoll) {
      window.clearInterval(eventPoll);
      eventPoll = null;
    }
    currentState = null;
  }

  async function init() {
    bindEvents();
    const params = new URLSearchParams(window.location.search);
    setupTokenFromURL = params.get("token") || "";
    if (setupTokenFromURL) {
      ui.setupToken.value = setupTokenFromURL;
      ui.setupTokenWrap.hidden = true;
      window.history.replaceState(null, "", window.location.pathname);
    }

    try {
      bootstrap = await api("/api/v1/bootstrap");
      ui.version.textContent = bootstrap.version ? "v" + bootstrap.version : "";
      if (bootstrap.setupRequired) {
        showSetup(bootstrap.localRequest);
        return;
      }
      if (window.location.pathname === "/setup") {
        window.history.replaceState(null, "", "/");
      }
      await enterDashboard();
    } catch (error) {
      setView("login");
      showNotice(friendlyError(error), "error");
    }
  }

  function showSetup(isLocal) {
    setView("setup");
    ui.setupFormWrap.hidden = !isLocal;
    if (!isLocal) {
      ui.setupLocalCopy.textContent = "Initial setup has not been completed. It must be done on the gaming PC itself; a phone or another LAN device cannot claim the administrator PIN.";
      return;
    }
    ui.setupLocalCopy.textContent = "Create the PIN that protects play permission and configuration. If the one-time setup link was not opened automatically, double-click the installed PlaygroundControl.exe and approve the Windows administrator prompt to refresh it.";
    if (document.activeElement !== ui.setupToken) {
      ui.setupPin.focus();
    }
  }

  async function enterDashboard() {
    const state = await api("/api/v1/state");
    csrfToken = state.csrfToken || "";
    currentState = state;
    setView("dashboard");
    renderState(state);
    startPolling();
    await loadEvents();
  }

  function showLogin(message) {
    stopDashboard();
    setView("login");
    if (message) {
      showNotice(message, "error");
    }
    window.setTimeout(() => ui.loginPin.focus(), 30);
  }

  async function refreshState(quiet) {
    if (refreshing || views.dashboard.hidden) {
      return;
    }
    refreshing = true;
    try {
      const state = await api("/api/v1/state");
      csrfToken = state.csrfToken || csrfToken;
      currentState = state;
      renderState(state);
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      } else if (!quiet) {
        showNotice(friendlyError(error), "error");
      }
    } finally {
      refreshing = false;
    }
  }

  function renderState(state) {
    currentState = state;
    const allowed = Boolean(state.playAllowed);
    ui.statusPanel.classList.toggle("allowed", allowed);
    ui.statusTitle.textContent = allowed ? "Play is allowed" : "Play is blocked";
    let detail;
    if (allowed) {
      detail = "Roblox runs normally. Configured games may run until a parent blocks play.";
    } else {
      detail = "Roblox is blocked before its process starts. Configured games are closed.";
    }
    ui.statusDetail.textContent = detail;
    ui.toggleButton.textContent = allowed ? "Block play" : "Allow play";
    ui.gamesList.replaceChildren();
    const customGames = state.games || [];
    ui.gameCount.textContent = String(customGames.length) + (customGames.length === 1 ? " custom game" : " custom games");
    ui.gamesEmpty.hidden = customGames.length > 0;
    renderBuiltinGuard(state);

    for (const game of customGames) {
      const row = document.createElement("div");
      row.className = "list-item";
      const copy = document.createElement("div");
      const title = document.createElement("span");
      title.className = "item-title";
      title.textContent = game.name;
      const meta = document.createElement("span");
      meta.className = "item-meta";
      meta.textContent = game.imagePath ? game.imagePath : game.executable + " (any folder)";
      copy.append(title, meta);
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "small-danger";
      remove.textContent = "Remove";
      remove.addEventListener("click", () => removeGame(game));
      row.append(copy, remove);
      ui.gamesList.append(row);
    }

    renderWarnings(state);
    renderEnforcement(state);
    renderLANURLs(state.lanUrls);
  }

  function renderBuiltinGuard(state) {
    const builtins = state.builtinGames || [];
    const armed = Boolean(state.launchGuardActive);
    ui.launchGuardBadge.textContent = armed ? "Armed" : "Disarmed";
    ui.launchGuardBadge.classList.toggle("disarmed", !armed);
    ui.builtinGamesList.replaceChildren();
    for (const game of builtins) {
      const chip = document.createElement("span");
      chip.className = "guard-chip";
      const name = document.createElement("strong");
      name.textContent = game.name;
      const executable = document.createElement("span");
      executable.textContent = game.executable;
      chip.append(name, executable);
      ui.builtinGamesList.append(chip);
    }
  }

  function renderWarnings(state) {
    ui.warnings.replaceChildren();
    const warnings = [];
    for (const warning of state.warnings || []) {
      warnings.push(warning);
    }
    if (state.pendingClosures && state.pendingClosures.length > 0) {
      warnings.push(state.pendingClosures.length + " game process" + (state.pendingClosures.length === 1 ? " is" : "es are") + " being asked to close.");
    }
    for (const text of warnings) {
      const box = document.createElement("div");
      box.className = "warning-box";
      box.textContent = text;
      ui.warnings.append(box);
    }
  }

  function renderEnforcement(state) {
    ui.enforcementList.replaceChildren();
    const pending = new Map();
    for (const item of state.pendingClosures || []) {
      pending.set(item.pid, item);
    }
    const running = state.runningBlockedGames || [];
    ui.enforcementEmpty.hidden = running.length > 0;
    for (const match of running) {
      const row = document.createElement("div");
      row.className = "list-item";
      const copy = document.createElement("div");
      const title = document.createElement("span");
      title.className = "item-title";
      title.textContent = match.gameName;
      const meta = document.createElement("span");
      meta.className = "item-meta";
      meta.textContent = match.name + " - PID " + match.pid;
      copy.append(title, meta);
      if (pending.has(match.pid)) {
        const badge = document.createElement("span");
        badge.className = "item-badge";
        badge.textContent = "Close requested";
        copy.append(badge);
      }
      row.append(copy);
      ui.enforcementList.append(row);
    }
  }

  function renderLANURLs(urls) {
    ui.lanUrls.replaceChildren();
    for (const address of urls || []) {
      const link = document.createElement("a");
      link.href = address;
      link.textContent = address;
      link.rel = "noreferrer";
      ui.lanUrls.append(link);
    }
  }

  async function loadEvents() {
    try {
      const response = await api("/api/v1/events?limit=50");
      const events = response.events || [];
      ui.eventsList.replaceChildren();
      ui.eventsEmpty.hidden = events.length > 0;
      for (const event of events) {
        const row = document.createElement("div");
        row.className = "event";
        const time = document.createElement("time");
        time.dateTime = event.time;
        time.textContent = formatTimestamp(event.time);
        const message = document.createElement("span");
        message.className = "event-message";
        message.textContent = event.message;
        row.append(time, message);
        ui.eventsList.append(row);
      }
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      }
    }
  }

  function formatTimestamp(value) {
    if (!value || value.startsWith("0001-")) {
      return "Not yet";
    }
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) {
      return value;
    }
    return date.toLocaleString();
  }

  async function togglePlay() {
    if (!currentState) {
      return;
    }
    const next = !currentState.playAllowed;
    if (!next) {
      const confirmed = window.confirm("Block play now? Every configured game that is running will be asked to close and may be force-closed after 10 seconds.");
      if (!confirmed) {
        return;
      }
    }
    setBusy(ui.toggleButton, true);
    clearNotice();
    try {
      const state = await api("/api/v1/play-state", { method: "PUT", body: { allowed: next } });
      csrfToken = state.csrfToken || csrfToken;
      renderState(state);
      showNotice(next ? "Play permission granted." : "Play permission revoked. Enforcement is active.", "success");
      await loadEvents();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      } else {
        showNotice(friendlyError(error), "error");
      }
    } finally {
      setBusy(ui.toggleButton, false);
    }
  }

  async function addGame(event) {
    event.preventDefault();
    const name = ui.gameName.value.trim();
    const executable = ui.gameExecutable.value.trim();
    const imagePath = ui.gamePath.value.trim();
    if (!name || !executable) {
      showNotice("A display name and executable name are required.", "error");
      return;
    }
    const submit = ui.gameForm.querySelector("button[type=submit]");
    setBusy(submit, true);
    clearNotice();
    try {
      await api("/api/v1/games", { method: "POST", body: { name, executable, imagePath } });
      ui.gameForm.reset();
      showNotice("Game added.", "success");
      await refreshState(false);
      await loadEvents();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      } else {
        showNotice(friendlyError(error), "error");
      }
    } finally {
      setBusy(submit, false);
    }
  }

  async function removeGame(game) {
    if (!window.confirm("Remove " + game.name + " from the blocked game list?")) {
      return;
    }
    clearNotice();
    try {
      await api("/api/v1/games/" + encodeURIComponent(game.id), { method: "DELETE" });
      showNotice("Game removed.", "success");
      await refreshState(false);
      await loadEvents();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      } else {
        showNotice(friendlyError(error), "error");
      }
    }
  }

  async function changePIN(event) {
    event.preventDefault();
    const currentPin = ui.currentPin.value;
    const newPin = ui.newPin.value;
    if (!PIN_PATTERN.test(currentPin) || !PIN_PATTERN.test(newPin)) {
      showNotice("PINs must contain 6 to 12 digits.", "error");
      return;
    }
    if (newPin !== ui.newPinConfirm.value) {
      showNotice("The new PIN confirmation does not match.", "error");
      return;
    }
    const submit = ui.pinForm.querySelector("button[type=submit]");
    setBusy(submit, true);
    clearNotice();
    try {
      await api("/api/v1/pin", { method: "PUT", body: { currentPin, newPin } });
      ui.pinForm.reset();
      showNotice("Administrator PIN changed.", "success");
      await loadEvents();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      } else {
        showNotice(friendlyError(error), "error");
      }
    } finally {
      setBusy(submit, false);
    }
  }

  async function logout() {
    clearNotice();
    try {
      await api("/api/v1/session", { method: "DELETE" });
    } catch (_error) {
      // Clear the local view even if the service session is already gone.
    }
    csrfToken = "";
    stopDashboard();
    ui.loginForm.reset();
    setView("login");
    ui.loginPin.focus();
  }

  async function setup(event) {
    event.preventDefault();
    const pin = ui.setupPin.value;
    const confirmPin = ui.setupPinConfirm.value;
    const token = ui.setupToken.value.trim();
    if (!PIN_PATTERN.test(pin)) {
      showNotice("Choose a PIN with 6 to 12 digits.", "error");
      return;
    }
    if (pin !== confirmPin) {
      showNotice("The PIN confirmation does not match.", "error");
      return;
    }
    if (!token) {
      showNotice("The one-time setup token is required. Run PlaygroundControl.exe as administrator to generate a fresh setup link.", "error");
      return;
    }
    const submit = ui.setupForm.querySelector("button[type=submit]");
    setBusy(submit, true);
    clearNotice();
    try {
      const response = await api("/api/v1/setup", { method: "POST", body: { token, pin } });
      csrfToken = response.csrfToken || "";
      ui.setupForm.reset();
      setupTokenFromURL = "";
      window.history.replaceState(null, "", "/");
      showNotice("Administrator PIN configured.", "success");
      await enterDashboard();
    } catch (error) {
      if (error instanceof APIError && error.status === 403 && error.code === "setup_token_invalid") {
        setupTokenFromURL = "";
        ui.setupToken.value = "";
        ui.setupTokenWrap.hidden = false;
      }
      showNotice(friendlyError(error), "error");
    } finally {
      setBusy(submit, false);
    }
  }

  async function login(event) {
    event.preventDefault();
    const pin = ui.loginPin.value;
    if (!PIN_PATTERN.test(pin)) {
      showNotice("Enter the 6 to 12 digit administrator PIN.", "error");
      return;
    }
    const submit = ui.loginForm.querySelector("button[type=submit]");
    setBusy(submit, true);
    clearNotice();
    try {
      const response = await api("/api/v1/session", { method: "POST", body: { pin } });
      csrfToken = response.csrfToken || "";
      ui.loginForm.reset();
      await enterDashboard();
    } catch (error) {
      if (error instanceof APIError && error.code === "setup_required") {
        const fresh = await api("/api/v1/bootstrap");
        showSetup(fresh.localRequest);
      }
      showNotice(friendlyError(error), "error");
    } finally {
      setBusy(submit, false);
    }
  }

  function bindEvents() {
    ui.setupForm.addEventListener("submit", setup);
    ui.loginForm.addEventListener("submit", login);
    ui.toggleButton.addEventListener("click", togglePlay);
    ui.gameForm.addEventListener("submit", addGame);
    ui.pinForm.addEventListener("submit", changePIN);
    ui.logoutButton.addEventListener("click", logout);
  }

  document.addEventListener("DOMContentLoaded", init);
})();
