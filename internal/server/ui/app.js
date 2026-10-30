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
    aiStatusBadge: document.getElementById("aiStatusBadge"),
    aiStatusText: document.getElementById("aiStatusText"),
    aiForm: document.getElementById("aiForm"),
    aiEnabled: document.getElementById("aiEnabled"),
    aiAutoBlock: document.getElementById("aiAutoBlock"),
    aiModel: document.getElementById("aiModel"),
    aiKey: document.getElementById("aiKey"),
    aiKeyState: document.getElementById("aiKeyState"),
    aiTestButton: document.getElementById("aiTestButton"),
    aiRemoveKeyButton: document.getElementById("aiRemoveKeyButton"),
    aiLastVerified: document.getElementById("aiLastVerified"),
    aiError: document.getElementById("aiError"),
    browserStatusBadge: document.getElementById("browserStatusBadge"),
    browserStatusText: document.getElementById("browserStatusText"),
    browserForm: document.getElementById("browserForm"),
    browserEnabled: document.getElementById("browserEnabled"),
    browserAutoBlock: document.getElementById("browserAutoBlock"),
    browserAI: document.getElementById("browserAI"),
    browserControlState: document.getElementById("browserControlState"),
    blockedDomainCount: document.getElementById("blockedDomainCount"),
    blockedDomainList: document.getElementById("blockedDomainList"),
    blockedDomainEmpty: document.getElementById("blockedDomainEmpty"),
    reviewDomainCount: document.getElementById("reviewDomainCount"),
    reviewDomainList: document.getElementById("reviewDomainList"),
    reviewDomainEmpty: document.getElementById("reviewDomainEmpty"),
    allowedDomainCount: document.getElementById("allowedDomainCount"),
    allowedDomainList: document.getElementById("allowedDomainList"),
    allowedDomainEmpty: document.getElementById("allowedDomainEmpty"),
    browserDetectionList: document.getElementById("browserDetectionList"),
    browserDetectionEmpty: document.getElementById("browserDetectionEmpty"),
    browserDomainForm: document.getElementById("browserDomainForm"),
    browserDomain: document.getElementById("browserDomain"),
    strangeActivityList: document.getElementById("strangeActivityList"),
    strangeActivityEmpty: document.getElementById("strangeActivityEmpty"),
    strangeCount: document.getElementById("strangeCount"),
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
  let strangePoll = null;
  let browserPoll = null;
  let refreshing = false;
  let setupTokenFromURL = "";
  let strangeActivity = [];
  let browserDomains = { blocked: [], review: [], allowed: [] };
  let browserDetections = [];
  let browserFormDirty = false;

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
    if (!strangePoll) {
      strangePoll = window.setInterval(() => {
        if (!document.hidden && !views.dashboard.hidden) {
          loadStrangeActivity();
        }
      }, 5000);
    }
    if (!browserPoll) {
      browserPoll = window.setInterval(() => {
        if (!document.hidden && !views.dashboard.hidden) {
          loadBrowserData();
        }
      }, 10000);
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
    if (strangePoll) {
      window.clearInterval(strangePoll);
      strangePoll = null;
    }
    if (browserPoll) {
      window.clearInterval(browserPoll);
      browserPoll = null;
    }
    currentState = null;
    strangeActivity = [];
    browserDomains = { blocked: [], review: [], allowed: [] };
    browserDetections = [];
    browserFormDirty = false;
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
    syncAIForm(state.ai || {});
    startPolling();
    await Promise.all([loadEvents(), loadStrangeActivity(), loadBrowserData()]);
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
      if (game.source === "ai") {
        const badge = document.createElement("span");
        badge.className = "item-badge ai";
        badge.textContent = "AI";
        copy.append(badge);
      }
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
    renderAIStatus(state.ai || {});
    renderBrowser(state.browser || {});
    renderLANURLs(state.lanUrls);
  }

  function renderAIStatus(ai) {
    const configured = Boolean(ai.keyConfigured);
    const enabled = Boolean(ai.enabled);
    let badge = "Off";
    let badgeClass = "off";
    if (enabled && configured && !ai.lastError) {
      badge = "On";
      badgeClass = "on";
    } else if (enabled && configured) {
      badge = "Error";
      badgeClass = "error";
    } else if (enabled) {
      badge = "Key missing";
      badgeClass = "error";
    }
    ui.aiStatusBadge.textContent = badge;
    ui.aiStatusBadge.className = "ai-badge " + badgeClass;
    if (!enabled) {
      ui.aiStatusText.textContent = "DeepSeek monitoring is off. No process data leaves this PC.";
    } else if (!configured) {
      ui.aiStatusText.textContent = "AI monitoring is enabled but no usable DeepSeek API key is configured.";
    } else if (ai.lastError) {
      ui.aiStatusText.textContent = "AI monitoring is enabled. DeepSeek is reporting an error.";
    } else if (ai.autoBlock) {
      ui.aiStatusText.textContent = "AI monitoring is watching while play is blocked. Games at 90% confidence or higher are auto-blocked and closed; lower-confidence findings need review.";
    } else {
      ui.aiStatusText.textContent = "AI monitoring is watching while play is blocked. Auto-block is off; findings need review.";
    }
    ui.aiKeyState.textContent = configured ? "API key configured, ending in " + (ai.keyHint || "****") + "." : "No API key configured.";
    ui.aiTestButton.disabled = !configured;
    ui.aiRemoveKeyButton.hidden = !configured;
    ui.aiLastVerified.textContent = ai.lastVerifiedAt && !String(ai.lastVerifiedAt).startsWith("0001-")
      ? "Last successful DeepSeek check: " + formatTimestamp(ai.lastVerifiedAt)
      : "";
    const showError = enabled && Boolean(ai.lastError);
    ui.aiError.hidden = !showError;
    ui.aiError.textContent = showError ? ai.lastError : "";
  }

  function syncAIForm(ai) {
    ui.aiEnabled.checked = Boolean(ai.enabled);
    ui.aiAutoBlock.checked = ai.autoBlock !== false;
    ui.aiModel.value = ai.model || "deepseek-flash";
    ui.aiKey.value = "";
    ui.aiKey.placeholder = ai.keyConfigured ? "Leave blank to keep the configured key" : "Enter a DeepSeek API key";
  }

  function renderBrowser(browser) {
    const settings = browser.settings || {};
    const status = browser.status || {};
    const enabled = Boolean(settings.enabled);
    let badge = "Off";
    let badgeClass = "off";
    if (enabled && status.lastError) {
      badge = "Error";
      badgeClass = "error";
    } else if (enabled) {
      badge = status.active ? "Active" : "Idle";
      badgeClass = "on";
    }
    ui.browserStatusBadge.textContent = badge;
    ui.browserStatusBadge.className = "ai-badge " + badgeClass;
    if (!enabled) {
      ui.browserStatusText.textContent = "Chrome and Edge history is not monitored. No browser domain data leaves this PC.";
    } else if (status.lastError) {
      ui.browserStatusText.textContent = "Browser monitoring is enabled but reporting an error.";
    } else if (currentState && currentState.playAllowed) {
      ui.browserStatusText.textContent = "Browser monitoring is enabled but idle while play is allowed. Policies and hosts entries are removed.";
    } else if (Number(browser.blockedCount) > 0 && (!status.urlBlocklistApplied || !status.hostsApplied)) {
      ui.browserStatusText.textContent = "Browser monitoring is watching, but controls are not fully active yet. The service retries automatically.";
    } else {
      ui.browserStatusText.textContent = "Watching Chrome and Edge history while play is blocked. Detection is retrospective; a game tab may stay open until it reloads or navigates.";
    }
    if (!browserFormDirty) {
      ui.browserEnabled.checked = enabled;
      ui.browserAutoBlock.checked = settings.autoBlock !== false;
      ui.browserAI.checked = Boolean(settings.aiClassification);
    }
    ui.blockedDomainCount.textContent = String(Number(browser.blockedCount) || 0);
    ui.reviewDomainCount.textContent = String(Number(browser.reviewCount) || 0);
    ui.allowedDomainCount.textContent = String(Number(browser.allowedCount) || 0);
    renderBrowserControlState(status, settings);
  }

  function renderBrowserControlState(status, settings) {
    ui.browserControlState.replaceChildren();
    const chips = [];
    const browsers = status.browsers || [];
    chips.push(browsers.length > 0 ? "Browsers: " + browsers.join(", ") : "No Chrome or Edge detected");
    chips.push(status.urlBlocklistApplied ? "URLBlocklist active" : "URLBlocklist inactive");
    chips.push(status.hostsApplied ? "Hosts active" : "Hosts inactive");
    chips.push(status.dohDisabled ? "DoH disabled" : "DoH unchanged");
    if (settings.lastScanAt && !String(settings.lastScanAt).startsWith("0001-")) {
      chips.push("Last scan " + formatTimestamp(settings.lastScanAt));
    }
    if (status.driftDetected) {
      chips.push("Drift repaired");
    }
    for (const text of chips) {
      const chip = document.createElement("span");
      chip.className = "state-chip";
      chip.textContent = text;
      ui.browserControlState.append(chip);
    }
  }

  async function loadBrowserData() {
    try {
      const [blocked, review, allowed, detections] = await Promise.all([
        api("/api/v1/browser/domains?status=blocked"),
        api("/api/v1/browser/domains?status=review"),
        api("/api/v1/browser/domains?status=allowed"),
        api("/api/v1/browser/detections?limit=50"),
      ]);
      browserDomains = {
        blocked: blocked.domains || [],
        review: review.domains || [],
        allowed: allowed.domains || [],
      };
      browserDetections = detections.detections || [];
      renderBrowserDomains();
      renderBrowserDetections();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      }
    }
  }

  function renderBrowserDomains() {
    renderBrowserDomainList(ui.blockedDomainList, ui.blockedDomainEmpty, browserDomains.blocked, "blocked");
    renderBrowserDomainList(ui.reviewDomainList, ui.reviewDomainEmpty, browserDomains.review, "review");
    renderBrowserDomainList(ui.allowedDomainList, ui.allowedDomainEmpty, browserDomains.allowed, "allowed");
  }

  function renderBrowserDomainList(container, emptyNode, items, listKind) {
    container.replaceChildren();
    emptyNode.hidden = items.length > 0;
    for (const item of items) {
      const row = document.createElement("div");
      row.className = "list-item domain-item";
      const copy = document.createElement("div");
      copy.className = "domain-copy";
      const title = document.createElement("span");
      title.className = "item-title";
      title.textContent = item.domain;
      const meta = document.createElement("span");
      meta.className = "item-meta";
      const source = item.source || "manual";
      const confidence = Math.round((Number(item.confidence) || 0) * 100);
      const evidence = source === "ai" && confidence > 0 ? confidence + "% confidence - " : "";
      meta.textContent = evidence + "last seen " + formatTimestamp(item.lastSeen) + " - seen " + (Number(item.seenCount) || 1) + (Number(item.seenCount) === 1 ? " time" : " times");
      const badge = document.createElement("span");
      badge.className = "item-badge " + source;
      badge.textContent = source === "seed" ? "Seed" : source === "ai" ? "AI" : "Manual";
      copy.append(title, meta, badge);
      if (item.reason) {
        const reason = document.createElement("span");
        reason.className = "item-meta strange-reason";
        reason.textContent = item.reason;
        copy.append(reason);
      }
      const actions = document.createElement("div");
      actions.className = "item-actions";
      if (listKind !== "allowed") {
        const allow = document.createElement("button");
        allow.type = "button";
        allow.className = "small-secondary";
        allow.textContent = "Allow";
        allow.addEventListener("click", () => setBrowserDomainStatus(item, "allowed", allow));
        actions.append(allow);
      }
      if (listKind !== "blocked") {
        const block = document.createElement("button");
        block.type = "button";
        block.className = "small-danger";
        block.textContent = "Block";
        block.addEventListener("click", () => setBrowserDomainStatus(item, "blocked", block));
        actions.append(block);
      }
      row.append(copy, actions);
      container.append(row);
    }
  }

  function renderBrowserDetections() {
    ui.browserDetectionList.replaceChildren();
    ui.browserDetectionEmpty.hidden = browserDetections.length > 0;
    const labels = {
      "seed-blocked": "Blocked by seed list",
      "ai-auto-blocked": "Auto-blocked by AI",
      "review": "Needs review",
      "already-blocked": "Already blocked",
      "existing": "Already handled",
    };
    for (const item of browserDetections) {
      const row = document.createElement("div");
      row.className = "event";
      const time = document.createElement("time");
      time.dateTime = item.time;
      time.textContent = formatTimestamp(item.time);
      const message = document.createElement("span");
      message.className = "event-message";
      const action = labels[item.action] || item.action || "Detected";
      message.textContent = item.domain + " - " + (item.browser || "browser") + " - " + action;
      row.append(time, message);
      ui.browserDetectionList.append(row);
    }
  }

  async function saveBrowserSettings(event) {
    event.preventDefault();
    const enabled = ui.browserEnabled.checked;
    const autoBlock = ui.browserAutoBlock.checked;
    const aiClassification = ui.browserAI.checked;
    if (aiClassification && !(currentState && currentState.ai && currentState.ai.keyConfigured)) {
      showNotice("Add a DeepSeek API key before enabling browser AI classification.", "error");
      return;
    }
    const submit = ui.browserForm.querySelector("button[type=submit]");
    setBusy(submit, true);
    clearNotice();
    try {
      const response = await api("/api/v1/browser/settings", {
        method: "PUT",
        body: { enabled, autoBlock, aiClassification },
      });
      browserFormDirty = false;
      if (currentState) {
        currentState.browser = response;
      }
      renderBrowser(response);
      showNotice("Browser monitoring settings saved.", "success");
      await refreshState(false);
      await Promise.all([loadBrowserData(), loadEvents()]);
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

  async function setBrowserDomainStatus(item, status, button) {
    const verb = status === "allowed" ? "allow" : "block";
    if (!window.confirm("Are you sure you want to " + verb + " " + item.domain + "?")) {
      return;
    }
    setBusy(button, true);
    clearNotice();
    try {
      await api("/api/v1/browser/domains/" + encodeURIComponent(item.id), {
        method: "PUT",
        body: { status },
      });
      showNotice(status === "allowed" ? "Browser domain allowed." : "Browser domain blocked.", "success");
      await refreshState(false);
      await Promise.all([loadBrowserData(), loadEvents()]);
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      } else {
        showNotice(friendlyError(error), "error");
      }
    } finally {
      setBusy(button, false);
    }
  }

  async function addBrowserDomain(event) {
    event.preventDefault();
    const domain = ui.browserDomain.value.trim();
    if (!domain) {
      showNotice("Enter a domain to block.", "error");
      return;
    }
    const submit = ui.browserDomainForm.querySelector("button[type=submit]");
    setBusy(submit, true);
    clearNotice();
    try {
      await api("/api/v1/browser/domains", { method: "POST", body: { domain } });
      ui.browserDomainForm.reset();
      showNotice("Browser domain blocked.", "success");
      await refreshState(false);
      await Promise.all([loadBrowserData(), loadEvents()]);
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

  async function loadStrangeActivity() {
    try {
      const response = await api("/api/v1/strange-activity?limit=50");
      strangeActivity = response.activity || [];
      renderStrangeActivity();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      }
    }
  }

  function renderStrangeActivity() {
    ui.strangeActivityList.replaceChildren();
    const ai = (currentState && currentState.ai) || {};
    ui.strangeCount.textContent = strangeActivity.length === 0
      ? "No findings"
      : String(strangeActivity.length) + (strangeActivity.length === 1 ? " finding" : " findings");
    ui.strangeActivityEmpty.hidden = strangeActivity.length > 0;
    if (!ai.enabled) {
      ui.strangeActivityEmpty.textContent = "AI monitoring is off. Enable it to detect unlisted games while play is blocked.";
    } else if (!ai.keyConfigured) {
      ui.strangeActivityEmpty.textContent = "AI monitoring is enabled but has no usable DeepSeek API key.";
    } else if (ai.autoBlock) {
      ui.strangeActivityEmpty.textContent = "No lower-confidence findings need review. High-confidence games are added directly to the blocked list.";
    } else {
      ui.strangeActivityEmpty.textContent = "No unexpected games detected yet.";
    }

    for (const item of strangeActivity) {
      const row = document.createElement("div");
      row.className = "list-item strange-item";
      const copy = document.createElement("div");
      copy.className = "strange-copy";
      const title = document.createElement("span");
      title.className = "item-title";
      title.textContent = item.name || item.executable;
      const identity = document.createElement("span");
      identity.className = "item-meta";
      identity.textContent = item.executable + (item.imagePath ? " - " + item.imagePath : " - path unavailable");
      const evidence = document.createElement("span");
      evidence.className = "item-meta";
      const confidence = Math.round((Number(item.confidence) || 0) * 100);
      const seen = Number(item.seenCount) || 1;
      evidence.textContent = confidence + "% confidence - first seen " + formatTimestamp(item.firstSeen) + " - last seen " + formatTimestamp(item.lastSeen) + " - seen " + seen + (seen === 1 ? " time" : " times");
      const reason = document.createElement("span");
      reason.className = "item-meta strange-reason";
      reason.textContent = item.reason || "";
      copy.append(title, identity, evidence, reason);

      const actions = document.createElement("div");
      actions.className = "item-actions";
      const add = document.createElement("button");
      add.type = "button";
      add.className = "small-secondary";
      add.textContent = "Add to blocked games";
      add.addEventListener("click", () => blockStrangeActivity(item, add));
      const ignore = document.createElement("button");
      ignore.type = "button";
      ignore.className = "small-danger";
      ignore.textContent = "Not a game";
      ignore.addEventListener("click", () => ignoreStrangeActivity(item, ignore));
      actions.append(add, ignore);
      row.append(copy, actions);
      ui.strangeActivityList.append(row);
    }
  }

  async function blockStrangeActivity(item, button) {
    const confirmed = window.confirm("Add " + item.name + " to the blocked game list? If play is blocked, its running process will be asked to close and may be force-closed after 10 seconds.");
    if (!confirmed) {
      return;
    }
    setBusy(button, true);
    clearNotice();
    try {
      await api("/api/v1/strange-activity/" + encodeURIComponent(item.id) + "/block", { method: "POST" });
      showNotice("Game added to the blocked list.", "success");
      await refreshState(false);
      syncAIForm((currentState && currentState.ai) || {});
      await Promise.all([loadEvents(), loadStrangeActivity(), loadBrowserData()]);
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      } else {
        showNotice(friendlyError(error), "error");
      }
    } finally {
      setBusy(button, false);
    }
  }

  async function ignoreStrangeActivity(item, button) {
    const confirmed = window.confirm("Mark " + (item.name || item.executable) + " as not a game? AI will ignore " + item.executable + " and this path in the future.");
    if (!confirmed) {
      return;
    }
    setBusy(button, true);
    clearNotice();
    try {
      await api("/api/v1/strange-activity/" + encodeURIComponent(item.id), { method: "DELETE" });
      showNotice("AI will ignore that executable and path.", "success");
      await loadStrangeActivity();
      await loadEvents();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      } else {
        showNotice(friendlyError(error), "error");
      }
    } finally {
      setBusy(button, false);
    }
  }

  async function saveAISettings(event) {
    event.preventDefault();
    const enabled = ui.aiEnabled.checked;
    const autoBlock = ui.aiAutoBlock.checked;
    const model = ui.aiModel.value.trim() || "deepseek-flash";
    const apiKey = ui.aiKey.value.trim();
    if (!/^[A-Za-z0-9._-]{1,64}$/.test(model)) {
      showNotice("The DeepSeek model name contains invalid characters.", "error");
      return;
    }
    if (apiKey && !/^\S{8,512}$/.test(apiKey)) {
      showNotice("The DeepSeek API key must be 8 to 512 non-whitespace characters.", "error");
      return;
    }
    if (enabled && !apiKey && !(currentState && currentState.ai && currentState.ai.keyConfigured)) {
      showNotice("Add a DeepSeek API key before enabling AI monitoring.", "error");
      return;
    }
    const submit = ui.aiForm.querySelector("button[type=submit]");
    setBusy(submit, true);
    clearNotice();
    try {
      const response = await api("/api/v1/ai-settings", {
        method: "PUT",
        body: { enabled, autoBlock, model, apiKey, clearApiKey: false },
      });
      ui.aiKey.value = "";
      if (response.ai) {
        syncAIForm(response.ai);
        renderAIStatus(response.ai);
        if (currentState) {
          currentState.ai = response.ai;
        }
      }
      showNotice("AI settings saved.", "success");
      await refreshState(false);
      syncAIForm((currentState && currentState.ai) || response.ai || {});
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

  async function testAIConnection() {
    if (!(currentState && currentState.ai && currentState.ai.keyConfigured)) {
      showNotice("Save a DeepSeek API key before testing the connection.", "error");
      return;
    }
    setBusy(ui.aiTestButton, true);
    clearNotice();
    try {
      const response = await api("/api/v1/ai-settings/test", { method: "POST" });
      showNotice("DeepSeek connection succeeded in " + response.latencyMs + " ms.", "success");
      await refreshState(false);
      syncAIForm((currentState && currentState.ai) || {});
      await loadEvents();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      } else {
        showNotice(friendlyError(error), "error");
      }
    } finally {
      setBusy(ui.aiTestButton, false);
    }
  }

  async function removeAIKey() {
    if (!window.confirm("Remove the DeepSeek API key and disable AI monitoring?")) {
      return;
    }
    const model = (currentState && currentState.ai && currentState.ai.model) || "deepseek-flash";
    const autoBlock = !(currentState && currentState.ai && currentState.ai.autoBlock === false);
    setBusy(ui.aiRemoveKeyButton, true);
    clearNotice();
    try {
      const response = await api("/api/v1/ai-settings", {
        method: "PUT",
        body: { enabled: false, autoBlock, model, apiKey: "", clearApiKey: true },
      });
      if (response.ai) {
        syncAIForm(response.ai);
        renderAIStatus(response.ai);
        if (currentState) {
          currentState.ai = response.ai;
        }
      }
      showNotice("DeepSeek API key removed. AI monitoring is off.", "success");
      await refreshState(false);
      syncAIForm((currentState && currentState.ai) || response.ai || {});
      await loadEvents();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        showLogin("Your session expired. Enter the administrator PIN again.");
      } else {
        showNotice(friendlyError(error), "error");
      }
    } finally {
      setBusy(ui.aiRemoveKeyButton, false);
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
    const prompt = game.source === "ai"
      ? "Remove " + game.name + " from the blocked game list? AI will ignore " + game.executable + " and this path and will not add it again."
      : "Remove " + game.name + " from the blocked game list?";
    if (!window.confirm(prompt)) {
      return;
    }
    clearNotice();
    try {
      await api("/api/v1/games/" + encodeURIComponent(game.id), { method: "DELETE" });
      showNotice(game.source === "ai" ? "AI game removed; AI will ignore it." : "Game removed.", "success");
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
    ui.aiForm.addEventListener("submit", saveAISettings);
    ui.browserForm.addEventListener("submit", saveBrowserSettings);
    for (const input of [ui.browserEnabled, ui.browserAutoBlock, ui.browserAI]) {
      input.addEventListener("change", () => { browserFormDirty = true; });
    }
    ui.browserDomainForm.addEventListener("submit", addBrowserDomain);
    ui.aiTestButton.addEventListener("click", testAIConnection);
    ui.aiRemoveKeyButton.addEventListener("click", removeAIKey);
    ui.pinForm.addEventListener("submit", changePIN);
    ui.logoutButton.addEventListener("click", logout);
  }

  document.addEventListener("DOMContentLoaded", init);
})();
