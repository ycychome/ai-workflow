(() => {
  "use strict";

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));
  const phaseNames = {
    INTAKE: "待处理", TRIAGE: "需求分析", SPEC_READY: "待确认方案", PLANNING: "规划中", DEVELOPING: "开发中",
    VERIFYING: "验证中", REVIEWING: "审查中", WAITING_CONFIGURATION: "等待配置", NEEDS_INFO: "需要补充信息",
    DEFERRED: "已暂缓", REJECT_PROPOSED: "方案已拒绝", CHANGES_REQUESTED: "待修复", BLOCKED: "受阻",
    AWAITING_MAINTAINER: "待维护者合并", MERGED: "已合并"
  };
  const controlNames = { ACTIVE: "可执行", PAUSED: "已暂停", CANCELLED: "已取消", TAKEOVER: "人工接管", COMPLETED: "已完成" };
  const commandNames = {
    status: "刷新状态", pause: "暂停", resume: "继续", cancel: "取消", replan: "重新规划",
    "review deep": "深度审查", takeover: "人工接管", accept: "接受结果", reject: "拒绝结果", defer: "稍后处理"
  };
  const healthNames = { database: "数据库", github: "GitHub", model: "模型服务", sandbox: "隔离执行" };
  const state = { workflows: [], health: {}, config: {}, selected: "", authenticated: false, settingsLoaded: false, auditEntries: [], busy: false };
  let toastTimer = 0;
  let pollTimer = 0;

  class APIError extends Error {
    constructor(message, status) { super(message); this.status = status; }
  }

  async function api(path, options = {}) {
    const response = await fetch(path, {
      credentials: "same-origin",
      ...options,
      headers: { Accept: "application/json", ...(options.body ? { "Content-Type": "application/json" } : {}), ...(options.headers || {}) }
    });
    const raw = await response.text();
    let payload = null;
    if (raw) {
      try { payload = JSON.parse(raw); } catch { payload = { message: raw }; }
    }
    if (response.status === 401) {
      showLogin();
      throw new APIError("登录已失效，请重新登录。", 401);
    }
    if (!response.ok) {
      const message = payload && (payload.error || payload.message || payload.detail);
      throw new APIError(typeof message === "string" ? message : `请求失败（HTTP ${response.status}）`, response.status);
    }
    return payload;
  }

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  function text(value, fallback = "—") {
    if (value === undefined || value === null || value === "") return fallback;
    return String(value);
  }

  function number(value) { return Number.isFinite(Number(value)) ? Number(value) : 0; }
  function phaseLabel(phase) { return phaseNames[phase] || text(phase, "未知阶段"); }
  function controlLabel(control) { return controlNames[control] || text(control, "未知"); }
  function formatTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }).format(date);
  }
  function formatNumber(value) { return new Intl.NumberFormat("zh-CN").format(number(value)); }
  function badgeClass(value) {
    if (["COMPLETED", "MERGED", "READY", "ACTIVE", "success", "satisfied", "RESOLVED"].includes(value)) return "badge-green";
    if (["DEVELOPING", "VERIFYING", "REVIEWING", "PLANNING", "TRIAGE"].includes(value)) return "badge-blue";
    if (["PAUSED", "CHANGES_REQUESTED", "BLOCKED", "medium", "high"].includes(value)) return "badge-amber";
    if (["FAILED", "CANCELLED", "critical", "open", "REJECTED"].includes(value)) return "badge-red";
    return "badge-muted";
  }
  function statusBadge(value, label = value) {
    return el("span", `badge ${badgeClass(value)}`, text(label));
  }

  function showToast(message, error = false) {
    const toast = $("#toast");
    toast.textContent = message;
    toast.style.background = error ? "#8e3642" : "#1d2b49";
    toast.classList.add("visible");
    window.clearTimeout(toastTimer);
    toastTimer = window.setTimeout(() => toast.classList.remove("visible"), 3400);
  }

  function showLogin() {
    state.authenticated = false;
    window.clearInterval(pollTimer);
    $("#app-view").hidden = true;
    $("#login-view").hidden = false;
    $("#login-token").value = "";
    $("#login-error").textContent = "";
    window.setTimeout(() => $("#login-token").focus(), 0);
  }

  function showApp() {
    state.authenticated = true;
    $("#login-view").hidden = true;
    $("#app-view").hidden = false;
  }

  function setRefreshState(value, failed = false) {
    const item = $("#refresh-state");
    item.textContent = value;
    item.style.color = failed ? "#c2525d" : "";
  }

  async function login(token) {
    await api("/api/login", { method: "POST", body: JSON.stringify({ token }) });
    $("#login-token").value = "";
    showApp();
    await refreshState();
    if (state.authenticated) {
      startPolling();
      await loadSettings(false);
    }
  }

  async function refreshState() {
    if (!state.authenticated && $("#login-view").hidden) return;
    try {
      const data = await api("/api/state");
      state.workflows = Array.isArray(data?.workflows) ? data.workflows : [];
      state.health = data?.health || {};
      state.config = data?.config || {};
      if (!state.selected || !state.workflows.some(w => w.id === state.selected)) state.selected = state.workflows[0]?.id || "";
      if (!state.authenticated) showApp();
      renderState();
      setRefreshState(`更新于 ${new Date().toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit" })}`);
    } catch (error) {
      if (error.status === 401) return;
      setRefreshState(error.message || "连接失败", true);
    }
  }

  function startPolling() {
    window.clearInterval(pollTimer);
    pollTimer = window.setInterval(refreshState, 3000);
  }

  function renderHealth(health) {
    const container = $("#health-strip");
    container.replaceChildren();
    const icons = { database: "▦", github: "⌘", model: "✦", sandbox: "⬡" };
    for (const key of ["database", "github", "model", "sandbox"]) {
      const value = health?.[key];
      const status = typeof value === "object" && value !== null ? (value.status ?? value.state ?? value.ok) : value;
      const detail = typeof value === "object" && value !== null ? (value.message ?? value.detail ?? value.status ?? "状态已检查") : value;
      const code = String(status).toLowerCase();
      const healthy = status === true || ["ok", "healthy", "ready", "connected", "configured", "available"].includes(code);
      const demoMode = code === "fake_mode";
      const missing = status === false || status === null || status === undefined || ["error", "failed", "unavailable", "missing", "not_configured"].includes(code);
      const card = el("div", `health-item ${healthy ? "ok" : missing ? "bad" : "warn"}`);
      card.append(el("span", "health-icon", icons[key]));
      const copy = el("div");
      const translated = typeof detail === "boolean" ? (detail ? "正常" : "未就绪") : ({ ok: "正常", healthy: "正常", ready: "就绪", configured: "已配置", not_configured: "未配置", fake_mode: "Fake 演示模式", unavailable: "不可用", error: "错误" }[code] || text(detail, "未知"));
      copy.append(el("strong", "", healthNames[key]), el("small", "", translated));
      if (typeof status === "string") card.title = status;
      card.append(copy, el("span", "health-status"));
      container.append(card);
    }
  }

  function renderStats(workflows) {
    const runnablePhases = new Set(["TRIAGE", "PLANNING", "DEVELOPING", "VERIFYING", "REVIEWING", "CHANGES_REQUESTED"]);
    const runnable = workflows.filter(w => w.control === "ACTIVE" && runnablePhases.has(w.phase)).length;
    const completed = workflows.filter(w => ["AWAITING_MAINTAINER", "MERGED"].includes(w.phase) || w.control === "COMPLETED").length;
    const errors = workflows.filter(w => Boolean(w.last_error) || ["FAILED", "BLOCKED"].includes(w.phase)).length;
    const calls = workflows.reduce((sum, w) => sum + number(w.model_calls), 0);
    const items = [
      ["工作流总数", workflows.length, "当前可见工作流", "▤"],
      ["可执行", runnable, "阶段可运行且控制状态为 ACTIVE", "◷"],
      ["待合并 / 已完成", completed, "等待维护者合并或已完成", "✓"],
      ["模型调用", formatNumber(calls), "累计调用次数", "✦"]
    ];
    const container = $("#stats-grid");
    container.replaceChildren();
    for (const [label, value, note, icon] of items) {
      const card = el("div", "stat-card");
      const top = el("div", "stat-top");
      top.append(el("span", "", label), el("span", "stat-icon", icon));
      card.append(top, el("div", "stat-value", value), el("div", "stat-note", label === "工作流总数" ? `${errors} 条需要关注` : note));
      container.append(card);
    }
  }

  function workflowTitle(workflow) {
    return workflow.issue_title || workflow.spec?.title || `Issue #${workflow.issue || "?"}`;
  }

  function workflowNameCell(workflow) {
    const wrap = el("div", "workflow-name");
    const repo = text(workflow.repository, "仓库");
    wrap.append(el("span", "repo-badge", repo.slice(0, 2).toUpperCase()));
    const label = el("div");
    label.append(el("strong", "", workflowTitle(workflow)), el("small", "", `${repo} · #${text(workflow.issue, "—")}`));
    wrap.append(label);
    return wrap;
  }

  function createWorkflowRow(workflow, detailed = false) {
    const row = document.createElement("tr");
    row.dataset.workflowId = workflow.id;
    if (workflow.id === state.selected) row.classList.add("selected");
    const first = document.createElement("td"); first.append(workflowNameCell(workflow)); row.append(first);
    const phase = document.createElement("td"); phase.append(statusBadge(workflow.phase, phaseLabel(workflow.phase))); row.append(phase);
    const control = document.createElement("td"); control.append(statusBadge(workflow.control, controlLabel(workflow.control))); row.append(control);
    if (detailed) {
      const gen = document.createElement("td"); gen.append(el("span", "numeric-cell", `G${text(workflow.generation, 0)} · ${workflow.pr ? `#${workflow.pr}` : "无 PR"}`)); row.append(gen);
      const calls = document.createElement("td"); calls.append(el("span", "numeric-cell", formatNumber(workflow.model_calls))); row.append(calls);
      const tasks = document.createElement("td"); tasks.append(el("span", "numeric-cell", `${workflow.tasks?.length || 0} 项`)); row.append(tasks);
    } else {
      const gen = document.createElement("td"); gen.append(el("span", "numeric-cell", `G${text(workflow.generation, 0)}`)); row.append(gen);
      const calls = document.createElement("td"); calls.append(el("span", "numeric-cell", formatNumber(workflow.model_calls))); row.append(calls);
    }
    const updated = document.createElement("td"); updated.textContent = formatTime(workflow.updated_at); row.append(updated);
    row.addEventListener("click", () => selectWorkflow(workflow.id));
    return row;
  }

  function renderOverview(workflows) {
    const body = $("#overview-rows"); body.replaceChildren();
    workflows.slice(0, 8).forEach(w => body.append(createWorkflowRow(w)));
    $("#overview-empty").hidden = workflows.length !== 0;
  }

  function populateFilters(workflows) {
    const phaseFilter = $("#phase-filter");
    const current = phaseFilter.value;
    const phases = [...new Set(workflows.map(w => w.phase).filter(Boolean))].sort();
    phaseFilter.replaceChildren(new Option("所有阶段", ""));
    for (const phase of phases) phaseFilter.add(new Option(phaseLabel(phase), phase));
    phaseFilter.value = phases.includes(current) ? current : "";

    const audit = $("#audit-workflow");
    const selected = audit.value;
    audit.replaceChildren(new Option("全部事件", ""));
    for (const w of workflows) audit.add(new Option(`${w.repository} · #${w.issue} · ${workflowTitle(w)}`, w.id));
    audit.value = workflows.some(w => w.id === selected) ? selected : "";
  }

  function renderWorkflows(workflows) {
    populateFilters(workflows);
    const query = $("#workflow-search").value.trim().toLowerCase();
    const phase = $("#phase-filter").value;
    const control = $("#control-filter").value;
    const visible = workflows.filter(w => {
      const searchable = `${w.repository || ""} ${w.issue || ""} ${workflowTitle(w)} ${w.id || ""}`.toLowerCase();
      return (!query || searchable.includes(query)) && (!phase || w.phase === phase) && (!control || w.control === control);
    });
    const body = $("#workflow-rows"); body.replaceChildren();
    visible.forEach(w => body.append(createWorkflowRow(w, true)));
    $("#workflow-empty").hidden = visible.length !== 0;
    renderWorkflowDetail(workflows.find(w => w.id === state.selected));
  }

  function selectWorkflow(id) {
    state.selected = id;
    renderOverview(state.workflows);
    renderWorkflows(state.workflows);
    if ($("#audit-workflow")) $("#audit-workflow").value = id;
  }

  function detailPair(list, label, value) {
    const item = el("li"); item.append(el("span", "", label), el("strong", "", text(value))); list.append(item);
  }

  function detailSection(title, count, content, full = false) {
    const section = el("section", `detail-section${full ? " full" : ""}`);
    const heading = el("h3"); heading.append(el("span", "", title), el("span", "", count === null ? "" : String(count)));
    section.append(heading, content);
    return section;
  }

  function criteriaList(spec) {
    const box = el("div", "criteria-list");
    const criteria = Array.isArray(spec?.criteria) ? spec.criteria : [];
    if (!criteria.length) return el("p", "muted", "暂无验收标准。");
    criteria.forEach((criterion, index) => {
      const row = el("div", "criterion-row");
      row.append(el("span", "criterion-id", text(criterion.id, String(index + 1))), el("p", "", text(criterion.description, "无描述")));
      box.append(row);
    });
    return box;
  }

  function renderWorkflowDetail(w) {
    const container = $("#workflow-detail");
    container.replaceChildren();
    if (!w) { container.append(el("div", "detail-placeholder", "选择一条工作流查看详情。")); return; }

    const heading = el("div", "detail-heading");
    const intro = el("div");
    intro.append(el("h2", "", workflowTitle(w)), el("p", "", `${text(w.repository)} · Issue #${text(w.issue)} · ${text(w.id)}`));
    const meta = el("div", "detail-meta");
    meta.append(statusBadge(w.phase, phaseLabel(w.phase)), statusBadge(w.control, controlLabel(w.control)), statusBadge("muted", `代次 ${text(w.generation, 0)}`));
    if (w.pr) meta.append(statusBadge("blue", `PR #${w.pr}`));
    intro.append(meta);
    const actions = el("div", "command-actions");
    for (const command of ["status", "pause", "resume", "replan", "review deep", "takeover", "accept", "reject", "defer", "cancel"]) {
      const button = el("button", "command-button", commandNames[command]);
      button.type = "button";
      button.addEventListener("click", () => sendCommand(w, command));
      actions.append(button);
    }
    heading.append(intro, actions);

    const grid = el("div", "detail-grid");
    const metrics = el("ul", "detail-list");
    detailPair(metrics, "生成代次", w.generation);
    detailPair(metrics, "模型调用", w.model_calls);
    detailPair(metrics, "输入 / 输出 Token", `${formatNumber(w.input_tokens)} / ${formatNumber(w.output_tokens)}`);
    detailPair(metrics, "运行时长", `${formatNumber(w.runtime_seconds)} 秒`);
    detailPair(metrics, "修复轮次", w.fix_rounds);
    detailPair(metrics, "PR / 分支", [w.pr ? `#${w.pr}` : "无 PR", w.branch].filter(Boolean).join(" · "));
    detailPair(metrics, "Base / Head", [w.base_sha, w.head_sha].filter(Boolean).join(" / "));
    detailPair(metrics, "更新时间", formatTime(w.updated_at));
    grid.append(detailSection("运行指标", null, metrics));

    const tasks = el("div");
    if (Array.isArray(w.tasks) && w.tasks.length) {
      for (const task of w.tasks) {
        const item = el("div", "item-card");
        const title = el("strong", "");
        title.append(document.createTextNode(`${text(task.id, "任务")} · `), statusBadge(task.status, text(task.status)));
        item.append(title, el("p", "", task.description || "无任务描述"));
        if (task.paths?.length) item.append(el("p", "", `文件：${task.paths.join("、")}`));
        item.append(el("p", "", `尝试 ${number(task.attempts)} 次${task.integrated_sha ? ` · Commit ${task.integrated_sha}` : ""}`));
        tasks.append(item);
      }
    } else tasks.append(el("p", "muted", "暂无任务记录。"));
    grid.append(detailSection("任务", w.tasks?.length || 0, tasks));

    const findings = el("div");
    const found = w.review?.findings || [];
    if (found.length) {
      for (const finding of found) {
        const item = el("div", "item-card");
        const line = el("strong", "");
        line.append(el("span", `finding-severity ${(finding.severity || "").toLowerCase()}`, text(finding.severity, "finding")), document.createTextNode(`${text(finding.id)} · ${text(finding.state)}`));
        item.append(line, el("p", "", `${text(finding.path)}${finding.line ? `:${finding.line}` : ""} · ${text(finding.description)}`));
        if (finding.expected || finding.actual) item.append(el("p", "", `预期：${text(finding.expected)}；实际：${text(finding.actual)}`));
        findings.append(item);
      }
    } else findings.append(el("p", "muted", "暂无发现项。"));
    grid.append(detailSection("审查发现", found.length, findings));

    const evidence = el("div");
    const evidenceItems = w.evidence || [];
    if (evidenceItems.length) {
      for (const itemData of evidenceItems) {
        const item = el("div", "item-card");
        item.append(el("strong", "", `${text(itemData.kind, "证据")} · ${formatTime(itemData.created_at)}`));
        item.append(el("p", "", `${text(itemData.path)} · SHA256 ${text(itemData.hash)}`));
        item.append(el("p", "", `Base ${text(itemData.base_sha)} → Head ${text(itemData.head_sha)}`));
        const view = el("button", "button button-secondary evidence-open", "查看证据内容");
        view.type = "button";
        view.disabled = !itemData.id;
        view.addEventListener("click", () => loadEvidence(w.id, itemData));
        item.append(view);
        evidence.append(item);
      }
    } else evidence.append(el("p", "muted", "暂无证据记录。"));
    grid.append(detailSection("验证证据", evidenceItems.length, evidence));

    const review = el("ul", "detail-list");
    detailPair(review, "评审结论", w.review?.verdict);
    detailPair(review, "评审理由", w.review?.reason);
    detailPair(review, "验证通过", w.review?.verification_passed ? "是" : "否");
    detailPair(review, "Skeptic 完成", w.review?.skeptic_complete ? "是" : "否");
    const reviewContent = el("div");
    reviewContent.append(review);
    const reviewTasks = w.review?.tasks || [];
    if (reviewTasks.length) {
      reviewContent.append(el("h4", "subsection-title", `评审任务 · ${reviewTasks.length}`));
      reviewTasks.forEach(task => {
        const item = el("div", "item-card");
        item.append(el("strong", "", `${text(task.id, "评审任务")} · ${text(task.status, "状态未知")}`));
        item.append(el("p", "", text(task.description, "无任务说明")));
        if (task.paths?.length) item.append(el("p", "", `覆盖文件：${task.paths.join("、")}`));
        item.append(el("p", "", `尝试 ${number(task.attempts)} 次${task.integrated_sha ? ` · Commit ${task.integrated_sha}` : ""}`));
        if (task.obligation_ids?.length) item.append(el("p", "", `对应义务：${task.obligation_ids.join("、")}`));
        if (task.report) item.append(el("pre", "inline-report", typeof task.report === "string" ? task.report : JSON.stringify(task.report, null, 2)));
        reviewContent.append(item);
      });
    }
    reviewContent.append(el("h4", "subsection-title", "当前已批准规格的验收标准"), criteriaList(w.spec));
    grid.append(detailSection("审查与验收", null, reviewContent));

    if (w.proposal_spec) {
      const proposal = el("div", "proposal-content");
      proposal.append(el("p", "proposal-state", w.proposal_spec.approved ? "该提案已批准" : "此方案仍待批准；批准前与当前规格分开展示。"));
      proposal.append(el("h4", "", text(w.proposal_spec.title, "未命名方案")));
      proposal.append(el("p", "", text(w.proposal_spec.description, "暂无方案说明")));
      proposal.append(el("strong", "subsection-title", "提案验收标准"), criteriaList(w.proposal_spec));
      if (w.proposal_spec.non_goals?.length) proposal.append(el("p", "", `非目标：${w.proposal_spec.non_goals.join("；")}`));
      grid.append(detailSection("待批准方案", w.proposal_spec.criteria?.length || 0, proposal, true));
    }

    if (w.last_error) {
      const errorBox = el("div", "error-box", w.last_error);
      grid.append(detailSection("最近错误", null, errorBox, true));
    }
    container.append(heading, grid);
  }

  async function sendCommand(workflow, command) {
    if (["cancel", "reject", "takeover"].includes(command) && !window.confirm(`确定对该工作流执行“${commandNames[command]}”吗？`)) return;
    try {
      await api(`/api/workflows/${encodeURIComponent(workflow.id)}/commands`, { method: "POST", body: JSON.stringify({ command }) });
      showToast(`已提交：${commandNames[command]}`);
      await refreshState();
      if (command === "status") await loadAudit(workflow.id);
    } catch (error) { showToast(error.message, true); }
  }

  function renderState() {
    renderHealth(state.health);
    renderStats(state.workflows);
    renderOverview(state.workflows);
    renderWorkflows(state.workflows);
    populateFilters(state.workflows);
  }

  async function loadAudit(workflowID = $("#audit-workflow").value) {
    const list = $("#audit-list");
    list.replaceChildren();
    const empty = $("#audit-empty");
    empty.hidden = true;
    try {
      const suffix = workflowID ? `?workflow_id=${encodeURIComponent(workflowID)}` : "";
      const result = await api(`/api/audit${suffix}`);
      state.auditEntries = Array.isArray(result?.entries) ? result.entries : [];
      for (const entry of state.auditEntries) {
        const row = el("article", "audit-entry");
        row.append(el("time", "audit-time", formatTime(entry.created_at)), el("div", "audit-kind", text(entry.kind, "事件")));
        const detail = el("details", "audit-data");
        const summary = el("summary", "", `查看数据 · ${text(entry.id, "无 ID")}`);
        const pre = el("pre", "", typeof entry.data === "string" ? entry.data : JSON.stringify(entry.data ?? {}, null, 2));
        detail.append(summary, pre);
        row.append(detail);
        list.append(row);
      }
      empty.hidden = state.auditEntries.length !== 0;
    } catch (error) {
      if (error.status !== 401) showToast(`审计读取失败：${error.message}`, true);
    }
  }

  async function loadEvidence(workflowID, evidence) {
    const dialog = $("#evidence-dialog");
    const title = $("#evidence-title");
    const body = $("#evidence-content");
    title.textContent = `${text(evidence.kind, "证据")} · ${text(evidence.path, "内容")}`;
    body.textContent = "正在读取证据…";
    dialog.hidden = false;
    try {
      const query = new URLSearchParams({ workflow_id: workflowID, evidence_id: evidence.id });
      const result = await api(`/api/evidence?${query}`);
      const content = result?.content;
      body.textContent = typeof content === "string" ? content : JSON.stringify(content ?? result?.evidence ?? result ?? {}, null, 2);
    } catch (error) {
      body.textContent = `读取失败：${error.message}`;
    }
  }

  function fillForm(form, config) {
    const github = config?.github || {};
    const model = config?.model || {};
    const runner = config?.runner || {};
    const budget = config?.budget || {};
    for (const name of ["base_url", "dev", "review", "max_output_tokens"]) {
      if (form.elements[name] && model[name] !== undefined) form.elements[name].value = model[name];
    }
    if (form.elements.stream) form.elements.stream.checked = Boolean(model.stream);
    if (form.elements.mode && runner.mode) form.elements.mode.value = runner.mode;
    for (const name of ["builders", "reviewers", "attempt_minutes"]) {
      if (form.elements[name] && runner[name] !== undefined) form.elements[name].value = runner[name];
    }
    for (const name of ["issue_calls", "daily_calls", "issue_minutes", "max_attempts", "max_fix_rounds"]) {
      if (form.elements[name] && budget[name] !== undefined) form.elements[name].value = budget[name];
    }
    for (const name of ["input_price_per_million", "output_price_per_million"]) {
      if (form.elements[name] && budget[name] !== undefined) form.elements[name].value = budget[name];
    }
    $("#github-public-url").value = github.public_url || "";
    // Secret fields are write-only; never reflect server-provided values into the page.
    form.elements.token.value = "";
  }

  async function loadSettings(showMessage = true) {
    const message = $("#settings-message");
    try {
      const data = await api("/api/settings");
      const config = data?.config || data || {};
      fillForm($("#settings-form"), config);
      state.settingsLoaded = true;
      renderGitHubIdentifiers(state.config);
      const model = config?.model || {};
      const ready = Boolean(model.base_url && model.dev && model.review);
      message.textContent = ready
        ? (showMessage ? "配置已加载。" : "")
        : "模型配置尚未完成：请填写 Base URL、开发模型和审查模型后保存。访问 Token 可稍后设置。";
      message.className = ready ? "form-message success" : "form-message";
    } catch (error) {
      if (error.status !== 401) {
        state.settingsLoaded = false;
        message.textContent = `配置加载失败：${error.message}`;
        message.className = "form-message";
      }
    }
  }

  function formNumber(form, name) {
    const raw = form.elements[name].value.trim();
    const value = Number(raw);
    if (raw === "" || !Number.isInteger(value)) throw new Error(`“${form.elements[name].closest("label")?.firstChild?.textContent || name}”必须填写整数。`);
    return value;
  }

  function formPrice(form, name) {
    const raw = form.elements[name].value.trim();
    const value = Number(raw);
    if (raw === "" || !Number.isFinite(value) || value < 0) throw new Error(`“${form.elements[name].closest("label")?.firstChild?.textContent || name}”必须填写不小于 0 的价格。`);
    return value;
  }

  async function saveSettings(event) {
    event.preventDefault();
    const form = event.currentTarget;
    const message = $("#settings-message");
    const submit = $("#save-settings");
    message.textContent = "";
    try {
      const payload = {
        model: {
          base_url: form.elements.base_url.value.trim(),
          dev: form.elements.dev.value.trim(),
          review: form.elements.review.value.trim(),
          stream: form.elements.stream.checked,
          max_output_tokens: formNumber(form, "max_output_tokens")
        },
        runner: {
          mode: form.elements.mode.value,
          builders: formNumber(form, "builders"),
          reviewers: formNumber(form, "reviewers"),
          attempt_minutes: formNumber(form, "attempt_minutes")
        },
        budget: {
          issue_calls: formNumber(form, "issue_calls"),
          daily_calls: formNumber(form, "daily_calls"),
          issue_minutes: formNumber(form, "issue_minutes"),
          max_attempts: formNumber(form, "max_attempts"),
          max_fix_rounds: formNumber(form, "max_fix_rounds"),
          input_price_per_million: formPrice(form, "input_price_per_million"),
          output_price_per_million: formPrice(form, "output_price_per_million")
        },
        github: { public_url: $("#github-public-url").value.trim() }
      };
      const token = form.elements.token.value;
      if (token) payload.model.token = token;
      submit.disabled = true;
      submit.textContent = "正在保存…";
      await api("/api/settings", { method: "POST", body: JSON.stringify(payload) });
      form.elements.token.value = "";
      message.textContent = "设置已保存。";
      message.className = "form-message success";
      showToast("设置已保存");
      await refreshState();
      renderGitHubIdentifiers(state.config);
    } catch (error) {
      message.textContent = `保存失败：${error.message}`;
      message.className = "form-message";
    } finally {
      submit.disabled = false;
      submit.textContent = "保存设置 →";
    }
  }

  async function runDoctor() {
    const button = $("#doctor-button");
    button.disabled = true;
    button.textContent = "正在诊断…";
    try {
      const result = await api("/api/doctor", { method: "POST", body: "{}" });
      const card = $("#doctor-result");
      card.hidden = false;
      card.replaceChildren(el("h3", "", "诊断结果"), el("pre", "", JSON.stringify(result, null, 2)));
      showToast("诊断完成，结果已显示在设置页。");
      await refreshState();
    } catch (error) { showToast(`诊断失败：${error.message}`, true); }
    finally { button.disabled = false; button.textContent = "✳ 运行诊断"; }
  }

  async function createDemo() {
    const button = $("#demo-button");
    button.disabled = true;
    button.textContent = "正在创建…";
    try {
      const response = await api("/api/demo", { method: "POST", body: "{}" });
      const workflow = response?.workflow || response;
      if (workflow?.id) state.selected = workflow.id;
      await refreshState();
      if (workflow?.id) selectWorkflow(workflow.id);
      showToast("已创建本地 Fake 演示工作流；不会调用 GitHub 或真实模型。");
    } catch (error) { showToast(`演示工作流创建失败：${error.message}`, true); }
    finally { button.disabled = false; button.textContent = "＋ 创建本地演示"; }
  }

  function renderGitHubIdentifiers(config) {
    const github = config?.github || {};
    const repos = Array.isArray(config?.repositories) ? config.repositories : [];
    const box = $("#github-identifiers");
    if (!box) return;
    box.replaceChildren();
    const appID = github.app_id || 0;
    const installationID = github.installation_id || 0;
    const lines = [
      ["App ID", appID ? String(appID) : "未配置"],
      ["Installation ID", installationID ? String(installationID) : "未配置"]
    ];
    for (const repo of repos) lines.push([text(repo.name, "仓库"), repo.id ? `Repository ID ${repo.id}` : "未绑定"]);
    if (!appID) lines.push(["状态", "尚未配置 GitHub App"]);
    else if (!installationID) lines.push(["状态", "App 已配置，尚未发现或绑定安装"]);
    lines.forEach(([label, value]) => {
      const row = el("div", "identifier-row");
      row.append(el("span", "", label), el("strong", "", value));
      box.append(row);
    });
  }

  async function discoverGitHub() {
    const button = $("#discover-github");
    const message = $("#github-message");
    button.disabled = true;
    message.textContent = "正在查询 GitHub App 安装…";
    try {
      const result = await api("/api/github/discover", { method: "POST", body: "{}" });
      const installations = Array.isArray(result?.installations) ? result.installations : [];
      const picker = $("#installation-picker");
      const select = $("#installation-select");
      select.replaceChildren(new Option(installations.length ? "请选择安装" : "未发现安装", ""));
      installations.forEach(installation => {
        const account = installation.account?.login || "GitHub 账户";
        select.add(new Option(`${account} · Installation ${installation.id} · App ${installation.app_id}`, installation.id));
      });
      picker.hidden = installations.length === 0;
      message.textContent = installations.length ? `发现 ${installations.length} 个安装。选择要授权给本服务的安装后再绑定。` : "当前 App 尚无可用安装；请先在 GitHub 安装 App 并选择仓库。";
    } catch (error) {
      message.textContent = `查询安装失败：${error.message}`;
    } finally { button.disabled = false; }
  }

  async function bindInstallation() {
    const select = $("#installation-select");
    const id = Number(select.value);
    const message = $("#github-message");
    if (!Number.isInteger(id) || id <= 0) { message.textContent = "请先选择一个安装。"; return; }
    const button = $("#bind-installation");
    button.disabled = true;
    message.textContent = "正在验证安装并绑定已授权仓库…";
    try {
      const result = await api("/api/github/discover", { method: "POST", body: JSON.stringify({ installation_id: id }) });
      const bound = (result?.repositories || []).filter(repo => repo.bound);
      const unbound = (result?.repositories || []).filter(repo => !repo.bound);
      message.textContent = `Installation ${result?.installation_id || id} 已绑定。${bound.length} 个配置仓库匹配，${unbound.length} 个尚未授权或 ID 不匹配。`;
      await refreshState();
      renderGitHubIdentifiers(state.config);
    } catch (error) {
      message.textContent = `绑定失败：${error.message}`;
    } finally { button.disabled = false; }
  }

  function setView(name) {
    for (const panel of $$(".view-panel")) panel.hidden = panel.dataset.panel !== name;
    for (const button of $$(".nav-link")) button.classList.toggle("active", button.dataset.view === name);
    const titles = { overview: "总览", workflows: "工作流", events: "事件审计", settings: "设置" };
    $("#page-title").textContent = titles[name] || "总览";
    if (name === "settings" && !state.settingsLoaded) loadSettings();
    if (name === "events") loadAudit();
    if (name === "workflows") renderWorkflows(state.workflows);
  }

  function wireEvents() {
    $("#login-form").addEventListener("submit", async event => {
      event.preventDefault();
      const token = $("#login-token").value;
      const error = $("#login-error");
      error.textContent = "";
      try { await login(token); }
      catch (reason) { error.textContent = reason.message; $("#login-token").value = ""; }
    });
    $$(".nav-link").forEach(button => button.addEventListener("click", () => setView(button.dataset.view)));
    $$("[data-go]").forEach(button => button.addEventListener("click", () => setView(button.dataset.go)));
    $("#refresh-button").addEventListener("click", refreshState);
    $("#workflow-search").addEventListener("input", () => renderWorkflows(state.workflows));
    $("#phase-filter").addEventListener("change", () => renderWorkflows(state.workflows));
    $("#control-filter").addEventListener("change", () => renderWorkflows(state.workflows));
    $("#audit-workflow").addEventListener("change", event => loadAudit(event.target.value));
    $("#audit-refresh").addEventListener("click", () => loadAudit());
    $("#settings-form").addEventListener("submit", saveSettings);
    $("#discover-github").addEventListener("click", discoverGitHub);
    $("#bind-installation").addEventListener("click", bindInstallation);
    $("#evidence-close").addEventListener("click", () => { $("#evidence-dialog").hidden = true; });
    $("#evidence-dialog").addEventListener("click", event => { if (event.target === event.currentTarget) event.currentTarget.hidden = true; });
    document.addEventListener("keydown", event => { if (event.key === "Escape") $("#evidence-dialog").hidden = true; });
    $("#doctor-button").addEventListener("click", runDoctor);
    $("#demo-button").addEventListener("click", createDemo);
  }

  wireEvents();
  showLogin();
  refreshState().then(() => {
    if (state.authenticated) startPolling();
  });
})();
