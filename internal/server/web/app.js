(() => {
  "use strict";

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));
  const phaseNames = {
    INTAKE: "待处理", TRIAGE: "需求分析", SPEC_READY: "待确认方案", PLANNING: "规划中", DEVELOPING: "开发中",
    VERIFYING: "验证中", REVIEWING: "审查中", WAITING_CONFIGURATION: "等待配置", NEEDS_INFO: "需要补充信息",
    DEFERRED: "已暂缓", REJECT_PROPOSED: "等待覆核的拒绝建议", REJECTED: "已拒绝", CHANGES_REQUESTED: "待修复", BLOCKED: "受阻",
    READY: "待远端检查", AWAITING_MAINTAINER: "待维护者合并", MERGED: "已合并"
  };
  const controlNames = { ACTIVE: "可执行", PAUSING: "暂停中", PAUSED: "已暂停", CANCELLING: "取消中", CANCELLED: "已取消", TAKEOVER: "人工接管", HUMAN_OWNED: "人工控制", COMPLETED: "已完成" };
  const commandNames = {
    status: "刷新状态", pause: "暂停", resume: "继续", cancel: "取消", replan: "重新规划",
    "review deep": "深度审查", takeover: "人工接管", accept: "接受结果", reject: "拒绝结果", defer: "稍后处理"
  };
  const healthNames = { database: "数据库", github: "GitHub", model: "模型服务", sandbox: "隔离执行" };
  const state = { workflows: [], health: {}, config: {}, selected: "", authenticated: false, settingsLoaded: false, auditEntries: [], route: null };
  let toastTimer = 0;
  let pollTimer = 0;
  let overviewSignature = "";
  let workflowListSignature = "";
  let githubInfoSignature = "";
  const evidenceViews = new Map();

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
  function taskLabel(status) { return ({QUEUED:"待执行",RUNNING:"执行中",VALIDATED:"产物已验证",INTEGRATED:"已集成",ACCEPTED:"审查已完成",RETRY_WAIT:"等待重试",BLOCKED:"受阻",SUPERSEDED:"已替代"})[status] || text(status,"未知状态"); }
  function gateLabel(status, conclusion) {
    const statusCode = String(status || "").toLowerCase();
    const conclusionCode = String(conclusion || "").toLowerCase();
    if (!statusCode && !conclusionCode) return "尚未确认";
    const names = {
      queued: "排队中", pending: "等待检查", in_progress: "检查进行中", completed: "检查已完成",
      success: "成功", pass: "成功", failure: "失败", failed: "失败", error: "错误",
      neutral: "中性", skipped: "已跳过", cancelled: "已取消", timed_out: "超时",
      action_required: "需要处理", stale: "已过期", startup_failure: "启动失败"
    };
    const statusText = statusCode ? (names[statusCode] || text(status, "")) : "";
    const conclusionText = conclusionCode ? (names[conclusionCode] || text(conclusion, "")) : "";
    return [statusText, conclusionText].filter(Boolean).join(" · ");
  }
  function formatTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }).format(date);
  }
  function formatNumber(value) { return new Intl.NumberFormat("zh-CN").format(number(value)); }
  function formatEstimatedCost(value) {
    return new Intl.NumberFormat("zh-CN", { minimumFractionDigits: 4, maximumFractionDigits: 6 }).format(value);
  }
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
      if (state.route?.page === "workflow-detail") state.selected = state.route.id;
      else if (state.selected && !state.workflows.some(w => w.id === state.selected)) state.selected = "";
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
      const translated = typeof detail === "boolean" ? (detail ? "正常" : "未就绪") : ({ ok: "正常", healthy: "正常", available: "可用", ready: "就绪", configured: "已配置", not_configured: "未配置", fake_mode: "Fake 演示模式", unavailable: "不可用", error: "错误" }[code] || text(detail, "未知"));
      copy.append(el("strong", "", healthNames[key]), el("small", "", translated));
      if (typeof status === "string") card.title = status;
      card.append(copy, el("span", "health-status"));
      container.append(card);
    }
  }

  function renderStats(workflows) {
    const runnablePhases = new Set(["TRIAGE", "PLANNING", "DEVELOPING", "VERIFYING", "REVIEWING", "CHANGES_REQUESTED"]);
    const runnable = workflows.filter(w => w.control === "ACTIVE" && runnablePhases.has(w.phase)).length;
    const completed = workflows.filter(w => w.phase === "MERGED" || w.control === "COMPLETED" || (w.phase === "AWAITING_MAINTAINER" && w.control === "ACTIVE" && w.review?.verdict === "PASS")).length;
    const errors = workflows.filter(w => Boolean(w.last_error) || ["FAILED", "BLOCKED"].includes(w.phase)).length;
    const calls = workflows.reduce((sum, w) => sum + number(w.model_calls), 0);
    const inputTokens = workflows.reduce((sum, w) => sum + number(w.input_tokens), 0);
    const outputTokens = workflows.reduce((sum, w) => sum + number(w.output_tokens), 0);
    const totalTokens = inputTokens + outputTokens;
    const budget = state.config?.budget || {};
    const inputPrice = number(budget.input_price_per_million);
    const outputPrice = number(budget.output_price_per_million);
    // Config documents zero prices as unknown, so avoid presenting them as free usage.
    const costText = inputPrice > 0 && outputPrice > 0
      ? `估算成本 ${formatEstimatedCost(inputTokens * inputPrice / 1_000_000 + outputTokens * outputPrice / 1_000_000)}（按配置价格单位）`
      : "成本单价未完整配置";
    const items = [
      ["工作流总数", workflows.length, "当前可见工作流", "▤"],
      ["可执行", runnable, "阶段可运行且控制状态为 ACTIVE", "◷"],
      ["待合并 / 已完成", completed, "等待维护者合并或已完成", "✓"],
      ["Token 使用量", formatNumber(totalTokens), `${costText} · 输入 ${formatNumber(inputTokens)} / 输出 ${formatNumber(outputTokens)} · ${formatNumber(calls)} 次调用`, "✦"]
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
    const signature = JSON.stringify({ selected: state.selected, rows: workflows.slice(0, 8).map(w => [w.id, w.phase, w.control, w.generation, w.model_calls, w.updated_at]) });
    if (signature === overviewSignature) return;
    overviewSignature = signature;
    const scrollY = window.scrollY;
    const tableScroll = $("#overview-rows").closest(".table-scroll");
    const scrollLeft = tableScroll.scrollLeft;
    const body = $("#overview-rows"); body.replaceChildren();
    workflows.slice(0, 8).forEach(w => body.append(createWorkflowRow(w)));
    $("#overview-empty").hidden = workflows.length !== 0;
    tableScroll.scrollLeft = scrollLeft;
    window.scrollTo({ top: scrollY, behavior: "auto" });
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
    const signature = JSON.stringify({ query, phase, control, selected: state.selected, rows: visible.map(w => [w.id, w.phase, w.control, w.generation, w.pr, w.model_calls, w.tasks?.length, w.updated_at]) });
    if (signature === workflowListSignature) return;
    workflowListSignature = signature;
    const scrollY = window.scrollY;
    const tableScroll = $("#workflow-rows").closest(".table-scroll");
    const scrollLeft = tableScroll.scrollLeft;
    const body = $("#workflow-rows"); body.replaceChildren();
    visible.forEach(w => body.append(createWorkflowRow(w, true)));
    $("#workflow-empty").hidden = visible.length !== 0;
    tableScroll.scrollLeft = scrollLeft;
    window.scrollTo({ top: scrollY, behavior: "auto" });
  }

  function selectWorkflow(id) {
    navigate(`/workflows/${encodeURIComponent(id)}/overview`);
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

  function reviewContent(w) {
    const content = el("div");
    const summary = el("ul", "detail-list");
    detailPair(summary, "本地评审结论", w.review?.verdict);
    detailPair(summary, "评审理由", w.review?.reason);
    detailPair(summary, "验证通过", w.review?.verification_passed ? "是" : "否");
    detailPair(summary, "Skeptic 完成", w.review?.skeptic_complete ? "是" : "否");
    content.append(summary);
    const reviewTasks = w.review?.tasks || [];
    content.append(el("h4", "subsection-title", `评审任务 · ${reviewTasks.length}`));
    if (reviewTasks.length) reviewTasks.forEach(task => {
      const item = el("div", "item-card");
      item.append(el("strong", "", `${text(task.id, "评审任务")} · ${taskLabel(task.status)}`));
      item.append(el("p", "", text(task.description, "无任务说明")));
      if (task.paths?.length) item.append(el("p", "", `覆盖文件：${task.paths.join("、")}`));
      item.append(el("p", "", `尝试 ${number(task.attempts)} 次${task.integrated_sha ? ` · Commit ${task.integrated_sha}` : ""}`));
      if (task.obligation_ids?.length) item.append(el("p", "", `对应义务：${task.obligation_ids.join("、")}`));
      if (task.report) item.append(reportDetails(task.report, "查看评审报告"));
      content.append(item);
    });
    const found = w.review?.findings || [];
    content.append(el("h4", "subsection-title", `审查发现 · ${found.length}`));
    if (found.length) found.forEach(finding => {
      const item = el("div", "item-card");
      const line = el("strong", "");
      line.append(el("span", `finding-severity ${(finding.severity || "").toLowerCase()}`, text(finding.severity, "发现项")), document.createTextNode(`${text(finding.id)} · ${text(finding.state)}`));
      item.append(line, el("p", "", `${text(finding.path)}${finding.line ? `:${finding.line}` : ""} · ${text(finding.description)}`));
      if (finding.expected || finding.actual) item.append(el("p", "", `预期：${text(finding.expected)}；实际：${text(finding.actual)}`));
      content.append(item);
    });
    if (!reviewTasks.length && !found.length) content.append(el("p", "muted", "暂无评审任务或发现项。"));
    content.append(el("h4", "subsection-title", "当前规格 · 验收标准"), criteriaList(w.spec));
    if (w.proposal_spec) {
      content.append(el("h4", "subsection-title", w.proposal_spec.approved ? "已批准方案" : "待批准方案"));
      const proposal = el("div", "proposal-content");
      proposal.append(el("h4", "", text(w.proposal_spec.title, "未命名方案")), el("p", "", text(w.proposal_spec.description, "暂无方案说明")));
      proposal.append(el("strong", "subsection-title", "提案验收标准"), criteriaList(w.proposal_spec));
      if (w.proposal_spec.non_goals?.length) proposal.append(el("p", "", `非目标：${w.proposal_spec.non_goals.join("；")}`));
      content.append(proposal);
    }
    return content;
  }

  function reportDetails(report, label) {
    const details = el("details", "report-details");
    const summary = el("summary", "", label);
    const content = typeof report === "string" ? report : JSON.stringify(report, null, 2);
    details.append(summary, el("pre", "inline-report", content));
    return details;
  }

  function currentEvidence(item, workflow) {
    const expected = String(workflow.tested_sha || workflow.head_sha || "").toLowerCase();
    const actual = String(item.tested_commit_sha || item.head_sha || "").toLowerCase();
    return Boolean(expected && actual && expected === actual);
  }

  function renderEvidenceTable(workflow, host) {
    const evidenceItems = Array.isArray(workflow.evidence) ? workflow.evidence : [];
    let view = evidenceViews.get(workflow.id);
    if (!view) {
      view = { kind: "", revision: "all", page: 1 };
      evidenceViews.set(workflow.id, view);
    }
    const perPage = 20;
    const types = [...new Set(evidenceItems.map(item => text(item.kind, "其他")))].sort();
    if (view.kind && !types.includes(view.kind)) view.kind = "";
    const filtered = evidenceItems.filter(item => {
      if (view.kind && text(item.kind, "其他") !== view.kind) return false;
      const isCurrent = currentEvidence(item, workflow);
      return view.revision === "all" || (view.revision === "current" ? isCurrent : !isCurrent);
    });
    const pages = Math.max(1, Math.ceil(filtered.length / perPage));
    view.page = Math.min(Math.max(1, view.page), pages);

    const toolbar = el("div", "evidence-toolbar");
    const count = el("span", "evidence-count", `共 ${filtered.length} 条${filtered.length !== evidenceItems.length ? ` / ${evidenceItems.length} 条` : ""}`);
    const kind = document.createElement("select");
    kind.setAttribute("aria-label", "按证据类型筛选");
    kind.append(new Option("所有类型", ""));
    types.forEach(type => kind.add(new Option(type, type)));
    kind.value = view.kind;
    kind.addEventListener("change", () => {
      view.kind = kind.value;
      view.page = 1;
      rerenderDetailTab(workflow, "evidence");
    });
    const revision = document.createElement("select");
    revision.setAttribute("aria-label", "筛选当前提交或历史证据");
    [["all", "所有提交"], ["current", "当前提交"], ["history", "历史提交"]].forEach(([value, label]) => revision.add(new Option(label, value)));
    revision.value = view.revision;
    revision.addEventListener("change", () => {
      view.revision = revision.value;
      view.page = 1;
      rerenderDetailTab(workflow, "evidence");
    });
    toolbar.append(count, kind, revision);

    const table = el("div", "evidence-table-wrap card");
    const scroll = el("div", "table-scroll");
    const tableNode = document.createElement("table");
    tableNode.className = "evidence-table";
    const head = document.createElement("thead");
    const headingRow = document.createElement("tr");
    ["类型", "时间", "命令或证据 ID", "提交", "状态", "查看"].forEach(label => headingRow.append(el("th", "", label)));
    head.append(headingRow);
    const body = document.createElement("tbody");
    const start = (view.page - 1) * perPage;
    for (const item of filtered.slice(start, start + perPage)) {
      const row = document.createElement("tr");
      row.append(el("td", "", text(item.kind, "其他")));
      row.append(el("td", "", formatTime(item.created_at)));
      const command = Array.isArray(item.command) ? item.command.join(" ") : "";
      row.append(el("td", "evidence-command", command || text(item.id)));
      const isCurrent = currentEvidence(item, workflow);
      row.append(el("td", "", isCurrent ? "当前提交" : "历史提交"));
      row.append(el("td", "", item.exit_code === 0 ? "成功（0）" : `失败（${text(item.exit_code, "未知")}）`));
      const action = document.createElement("td");
      const open = el("button", "evidence-open-row", "查看");
      open.type = "button";
      open.disabled = !item.id;
      open.addEventListener("click", () => loadEvidence(workflow.id, item));
      action.append(open);
      row.append(action);
      body.append(row);
    }
    tableNode.append(head, body);
    scroll.append(tableNode);
    table.append(scroll);
    if (!filtered.length) table.append(el("div", "empty-state", "没有符合筛选条件的证据。"));

    const pagination = el("div", "evidence-pagination");
    const previous = el("button", "button button-secondary", "← 上一页");
    previous.type = "button";
    previous.disabled = view.page <= 1;
    previous.addEventListener("click", () => { view.page--; rerenderDetailTab(workflow, "evidence"); });
    const next = el("button", "button button-secondary", "下一页 →");
    next.type = "button";
    next.disabled = view.page >= pages;
    next.addEventListener("click", () => { view.page++; rerenderDetailTab(workflow, "evidence"); });
    pagination.append(previous, el("span", "evidence-page-number", `第 ${view.page} / ${pages} 页`), next);
    host.append(toolbar, table, pagination);
  }

  function rerenderDetailTab(workflow, tab) {
    const container = $("#workflow-detail");
    delete container.dataset.renderSignature;
    renderWorkflowDetail(workflow, tab);
  }

  function flowStatus(w, node) {
    const phase = w.phase || "";
    const control = w.control || "";
    const stage = ({ INTAKE: 0, WAITING_CONFIGURATION: 1, TRIAGE: 2, NEEDS_INFO: 2, REJECT_PROPOSED: 2,
      REJECTED: 2, DEFERRED: 2, SPEC_READY: 3, PLANNING: 4, DEVELOPING: 5, CHANGES_REQUESTED: 5,
      VERIFYING: 6, REVIEWING: 7, READY: 10, AWAITING_MAINTAINER: 10, MERGED: 11 })[phase] ??
      (phase === "BLOCKED" ? (w.review?.verification_passed ? 7 : w.tasks?.length ? 5 : 2) : 0);
    const cancelled = ["CANCELLING", "CANCELLED"].includes(control);
    const terminalProposal = phase === "REJECT_PROPOSED";
    const terminalStopped = ["REJECTED", "DEFERRED"].includes(phase);
    const gateIsCurrent = Boolean(w.check_sha && w.head_sha && w.check_sha.toLowerCase() === w.head_sha.toLowerCase());
    const reviewCurrent = w.review?.verdict !== "OBSOLETE";
    const reviewEligibleForGate = flowReviewEligibleForGate(w);
    const remoteGatePassed = control === "ACTIVE" && gateIsCurrent && String(w.gate_status).toLowerCase() === "completed" && String(w.gate_conclusion).toLowerCase() === "success";
    const gatePassed = remoteGatePassed && reviewEligibleForGate;
    const reviewerTasks = Array.isArray(w.review?.tasks) ? w.review.tasks : [];
    const reviewersDone = reviewCurrent && reviewerTasks.length >= 2 && reviewerTasks.every(task => task.status === "ACCEPTED");
    const firstIncomplete = reviewerTasks.findIndex(task => task.status !== "ACCEPTED");
    const currentNode = phase === "WAITING_CONFIGURATION" ? "eligibility"
      : phase === "INTAKE" ? "eligibility"
        : ["TRIAGE", "NEEDS_INFO"].includes(phase) ? "triage"
          : phase === "SPEC_READY" ? "spec"
            : phase === "PLANNING" ? "plan"
              : ["DEVELOPING", "CHANGES_REQUESTED"].includes(phase) ? (phase === "CHANGES_REQUESTED" ? "fix" : "build")
                : phase === "VERIFYING" ? "verify"
        : phase === "REVIEWING" ? (reviewersDone ? (w.review?.skeptic_complete ? "gate" : "skeptic") : firstIncomplete <= 1 ? (firstIncomplete === 1 ? "reviewer2" : "reviewer1") : "reviewer2")
          : phase === "BLOCKED" ? (w.review?.verification_passed ? reviewersDone ? (w.review?.skeptic_complete ? "gate" : "skeptic") : "reviewer1" : w.tasks?.length ? "verify" : "plan")
            : phase === "READY" ? "gate" : ["AWAITING_MAINTAINER", "MERGED"].includes(phase) ? "human" : terminalProposal ? "maintainer-review" : "";
    let status = "pending";
    const completedByStage = {
      issue: true,
      eligibility: stage >= 2 && phase !== "WAITING_CONFIGURATION",
      triage: stage >= 3 || ["NEEDS_INFO", "REJECT_PROPOSED", "REJECTED", "DEFERRED"].includes(phase),
      spec: Boolean(w.spec?.approved),
      plan: stage >= 5,
      build: stage >= 6,
      verify: Boolean(w.review?.verification_passed) && reviewCurrent,
      reviewer1: reviewerTasks[0]?.status === "ACCEPTED" && reviewCurrent,
      reviewer2: reviewerTasks[1]?.status === "ACCEPTED" && reviewCurrent,
      skeptic: Boolean(w.review?.skeptic_complete) && reviewCurrent,
      gate: gatePassed,
      human: phase === "MERGED",
      merged: phase === "MERGED"
    };
    if (completedByStage[node]) status = "done";
    if (node === "reviewer1" && reviewerTasks[0]?.status === "RUNNING") status = "active";
    if (node === "reviewer2" && reviewerTasks[1]?.status === "RUNNING") status = "active";
    if (node === "reviewer1" && reviewerTasks[0]?.status === "BLOCKED" || node === "reviewer2" && reviewerTasks[1]?.status === "BLOCKED") status = "blocked";
    if (node === "skeptic" && phase === "REVIEWING" && reviewersDone && !w.review?.skeptic_complete) status = "active";
    if (node === "gate" && gateIsCurrent && String(w.gate_status).toLowerCase() === "in_progress") status = "active";
    if (node === currentNode && status !== "done") {
      if (control === "PAUSED" || control === "PAUSING") status = "paused";
      else if (control === "BLOCKED" || phase === "BLOCKED") status = "blocked";
      else if (control === "HUMAN_OWNED") status = "human";
      else if (cancelled) status = "cancelled";
      else if (control === "ACTIVE") status = "active";
    }
    if (node === "maintainer-review" && terminalProposal) status = "active";
    if (node === "hold" && phase === "WAITING_CONFIGURATION") status = control === "ACTIVE" ? "active" : "paused";
    if (node === "needs-info" && phase === "NEEDS_INFO") status = control === "ACTIVE" ? "active" : "paused";
    if (node === "fix" && phase === "CHANGES_REQUESTED") status = control === "ACTIVE" ? "active" : control === "PAUSED" ? "paused" : "pending";
    if (node === "merged" && phase === "MERGED") status = "done";
    if (cancelled && !completedByStage[node] && node === currentNode) status = "cancelled";
    if ((terminalProposal || terminalStopped) && !["issue", "eligibility", "triage", "maintainer-review"].includes(node)) status = "pending";
    if (gatePassed && node === "gate") status = "done";
    if (node === "gate" && gateIsCurrent && String(w.gate_status).toLowerCase() === "completed" && String(w.gate_conclusion).toLowerCase() !== "success") status = "blocked";
    if (node === "gate" && w.check_sha && w.head_sha && !gateIsCurrent) status = "blocked";
    if (node === "gate" && remoteGatePassed && !reviewEligibleForGate) status = "stale";
    if (reviewCurrent === false && ["reviewer1", "reviewer2", "skeptic", "verify"].includes(node) &&
        ((node === "reviewer1" && reviewerTasks[0]?.status === "ACCEPTED") ||
          (node === "reviewer2" && reviewerTasks[1]?.status === "ACCEPTED") ||
          (node === "skeptic" && w.review?.skeptic_complete) ||
          (node === "verify" && w.review?.verification_passed))) status = "stale";
    return status;
  }

  function renderWorkflowDiagram(w) {
    const phase = w.phase || "";
    const terminalProposal = phase === "REJECT_PROPOSED";
    const section = el("section", "workflow-flow-section card");
    const header = el("div", "flow-header");
    const title = el("div");
    title.append(el("h2", "", "工作流生命周期"), el("p", "muted", `${text(w.repository)} · Issue #${text(w.issue)} · Workflow ${text(w.id)} · 当前阶段：${phaseLabel(w.phase)}`));
    const controlNotes = {
      PAUSING: "正在暂停", PAUSED: "已暂停", BLOCKED: "受阻", CANCELLING: "正在取消",
      CANCELLED: "已取消", HUMAN_OWNED: "人工控制", COMPLETED: "已完成"
    };
    const needsAttention = Boolean(controlNotes[w.control]) || w.phase === "BLOCKED";
    const summary = el("div", "flow-state-summary");
    summary.append(statusBadge("muted", `当前位置：${flowCurrentLabel(w)}`));
    summary.append(statusBadge(needsAttention ? "BLOCKED" : "muted", controlNotes[w.control] || (w.phase === "BLOCKED" ? "流程受阻" : controlLabel(w.control))));
    header.append(title, summary);
    const scroller = el("div", "flow-scroll");
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("viewBox", "0 0 1700 300");
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", `${w.repository} Issue ${w.issue} 工作流状态图：${phaseLabel(w.phase)}，${controlLabel(w.control)}`);
    svg.classList.add("workflow-flow-svg");
    const titleNode = document.createElementNS(svg.namespaceURI, "title");
    titleNode.textContent = `${text(w.repository)} · Issue #${text(w.issue)} 生命周期`;
    svg.append(titleNode);
    const defs = document.createElementNS(svg.namespaceURI, "defs");
    const marker = document.createElementNS(svg.namespaceURI, "marker");
    marker.id = "workflow-flow-arrow";
    marker.setAttribute("viewBox", "0 0 10 10"); marker.setAttribute("refX", "8"); marker.setAttribute("refY", "5");
    marker.setAttribute("markerWidth", "6"); marker.setAttribute("markerHeight", "6"); marker.setAttribute("orient", "auto-start-reverse");
    const arrow = document.createElementNS(svg.namespaceURI, "path");
    arrow.setAttribute("d", "M 0 0 L 10 5 L 0 10 z");
    arrow.setAttribute("fill", "currentColor"); marker.append(arrow); defs.append(marker); svg.append(defs);

    const x = { issue: 24, eligibility: 152, triage: 280, spec: 408, plan: 536, build: 664, verify: 792,
      reviewer1: 920, reviewer2: 920, skeptic: 1048, gate: 1176, human: 1304, merged: 1432,
      hold: 152, "needs-info": 280, "maintainer-review": 408, fix: 1176 };
    const y = { issue: 68, eligibility: 68, triage: 68, spec: 68, plan: 68, build: 68, verify: 68,
      reviewer1: 42, reviewer2: 132, skeptic: 87, gate: 87, human: 87, merged: 87,
      hold: 208, "needs-info": 208, "maintainer-review": 208, fix: 208 };
    const width = 112, height = 46;
    const connector = (d, cls = "") => {
      const path = document.createElementNS(svg.namespaceURI, "path");
      path.setAttribute("d", d); path.setAttribute("class", `flow-connector ${cls}`.trim());
      path.setAttribute("marker-end", "url(#workflow-flow-arrow)"); svg.append(path);
    };
    const centerY = key => y[key] + height / 2;
    const right = key => x[key] + width;
    const left = key => x[key];
    const centerX = key => x[key] + width / 2;
    const straight = (a, b) => connector(`M ${right(a)} ${centerY(a)} H ${left(b)}`);
    const edgeLabel = (label, px, py) => {
      const textNode = document.createElementNS(svg.namespaceURI, "text");
      textNode.setAttribute("x", String(px)); textNode.setAttribute("y", String(py)); textNode.setAttribute("class", "flow-edge-label");
      textNode.setAttribute("text-anchor", "middle"); textNode.textContent = label; svg.append(textNode);
    };
    [["issue", "eligibility"], ["eligibility", "triage"], ["triage", "spec"], ["spec", "plan"], ["plan", "build"], ["build", "verify"]].forEach(([a, b]) => straight(a, b));
    edgeLabel("允许", 140, 58); edgeLabel("符合资格", 268, 58); edgeLabel("接受", 396, 58);
    connector(`M ${right("verify")} ${centerY("verify")} H 895 V ${centerY("reviewer1")} H ${left("reviewer1")}`);
    connector(`M 895 ${centerY("reviewer1")} V ${centerY("reviewer2")} H ${left("reviewer2")}`);
    connector(`M ${right("reviewer1")} ${centerY("reviewer1")} H 1030 V ${centerY("skeptic")} H ${left("skeptic")}`);
    connector(`M ${right("reviewer2")} ${centerY("reviewer2")} H 1030 V ${centerY("skeptic")} H ${left("skeptic")}`);
    [["skeptic", "gate"], ["gate", "human"], ["human", "merged"]].forEach(([a, b]) => straight(a, b));
    edgeLabel("审查完成", 1038, 76); edgeLabel("通过", 1296, 77); edgeLabel("维护者合并", 1424, 77);
    connector(`M ${centerX("triage")} ${y.triage + height} V ${y["needs-info"]}`);
    edgeLabel("需澄清", 350, 164);
    connector(`M ${centerX("eligibility")} ${y.eligibility + height} V ${y.hold}`);
    edgeLabel("否", 295, 160);
    connector(`M ${left("needs-info")} ${centerY("needs-info")} H 260 V ${centerY("triage")} H ${left("triage")}`, "flow-loop");
    connector(`M ${centerX("triage")} ${y.triage + height} V 182 H ${centerX("maintainer-review")} V ${y["maintainer-review"]}`);
    edgeLabel("拒绝 / 延期建议", 397, 174);
    connector(`M ${right("maintainer-review")} ${centerY("maintainer-review")} H ${centerX("spec")} V ${y.spec + height}`, "flow-loop");
    connector(`M ${right("gate")} ${centerY("gate")} H 1296 V 182 H ${centerX("maintainer-review")} V ${y["maintainer-review"]}`);
    edgeLabel("覆盖不足 / 争议", 850, 174);
    connector(`M ${centerX("gate")} ${y.gate + height} V ${y.fix}`);
    edgeLabel("需修改", 1260, 166);
    connector(`M ${left("fix")} ${centerY("fix")} H 520 V 172 H ${centerX("plan")} V ${y.plan + height}`, "flow-loop");
    connector(`M ${centerX("human")} ${y.human + height} V 284 H ${centerX("plan")} V ${y.plan + height}`, "flow-loop");
    edgeLabel("修改", 970, 279);

    const nodes = [
      ["issue", "Issue 入口", "issue", "创建 / 更新"], ["eligibility", "授权与预算", "eligibility", "入队资格"],
      ["triage", "需求确认", "triage", "Triage"], ["spec", "批准规格", "spec", w.spec?.approved ? "已批准" : w.proposal_spec ? "待确认提案" : "等待"],
      ["plan", "开发计划", "plan", `${(w.tasks || []).length} 项任务`], ["build", "开发任务", "build", "Builder"],
      ["verify", "可信验证", "verify", reviewMilestoneLabel(w, "verify")],
      ["reviewer1", "Reviewer 1", "reviewer1", reviewMilestoneLabel(w, "reviewer1")],
      ["reviewer2", "Reviewer 2", "reviewer2", reviewMilestoneLabel(w, "reviewer2")],
      ["skeptic", "Skeptic", "skeptic", reviewMilestoneLabel(w, "skeptic")],
      ["gate", "合并门禁", "gate", gatePassedLabel(w)], ["human", "待人工合并", "human", w.pr_ready ? "Ready for review" : "等待复核"],
      ["merged", "已合并", "merged", phaseNames[w.phase] === "已合并" ? "已记录" : "待合并"],
      ["hold", "记录 / 等待授权", "hold", phase === "WAITING_CONFIGURATION" ? "等待配置" : "未获准时不入队"],
      ["needs-info", "补充信息", "needs-info", phase === "NEEDS_INFO" ? "等待回复" : "需求澄清分支"],
      ["maintainer-review", "维护者覆核", "maintainer-review", terminalProposal ? phaseLabel(w.phase) : "争议 / 延期建议"],
      ["fix", "修复 / 反证", "fix", phase === "CHANGES_REQUESTED" ? `第 ${number(w.fix_rounds)} 轮` : "回到计划"]
    ];
    const nodeLinks = { plan: "development", build: "development", verify: "evidence", reviewer1: "review", reviewer2: "review", skeptic: "review", gate: "review", fix: "development" };
    for (const [key, label, statusKey, detail] of nodes) {
      const group = document.createElementNS(svg.namespaceURI, "g");
      group.setAttribute("class", `flow-node is-${flowStatus(w, statusKey)}`);
      const status = flowStatus(w, statusKey);
      const accessible = `${label}，${detail}，${({ done: "已完成", active: "当前步骤", pending: "待执行", paused: "已暂停", blocked: "受阻", human: "人工控制", cancelled: "已取消", stale: "历史 / 待重审" })[status] || status}`;
      group.setAttribute("aria-label", accessible);
      if (nodeLinks[key]) {
        group.setAttribute("role", "link"); group.setAttribute("tabindex", "0"); group.dataset.flowTab = nodeLinks[key];
        const go = () => navigate(`/workflows/${encodeURIComponent(w.id)}/${nodeLinks[key]}`);
        group.addEventListener("click", go);
        group.addEventListener("keydown", event => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); go(); } });
      }
      const nodeTitle = document.createElementNS(svg.namespaceURI, "title"); nodeTitle.textContent = accessible; group.append(nodeTitle);
      const rect = document.createElementNS(svg.namespaceURI, "rect");
      rect.setAttribute("x", String(x[key])); rect.setAttribute("y", String(y[key])); rect.setAttribute("width", String(width)); rect.setAttribute("height", String(height)); rect.setAttribute("rx", "8"); group.append(rect);
      const textNode = document.createElementNS(svg.namespaceURI, "text"); textNode.setAttribute("x", String(centerX(key))); textNode.setAttribute("y", String(y[key] + 19)); textNode.setAttribute("text-anchor", "middle");
      const labelLine = document.createElementNS(svg.namespaceURI, "tspan"); labelLine.setAttribute("x", String(centerX(key))); labelLine.setAttribute("class", "flow-node-label"); labelLine.textContent = label;
      const detailLine = document.createElementNS(svg.namespaceURI, "tspan"); detailLine.setAttribute("x", String(centerX(key))); detailLine.setAttribute("dy", "15"); detailLine.setAttribute("class", "flow-node-detail"); detailLine.textContent = detail;
      textNode.append(labelLine, detailLine); group.append(textNode); svg.append(group);
    }
    scroller.append(svg);
    const note = el("p", "flow-note", flowStateNote(w));
    const legend = el("div", "flow-legend");
    [["done", "已完成"], ["active", "当前步骤"], ["pending", "待执行"], ["paused", "暂停中"], ["blocked", "受阻 / 待确认"], ["stale", "历史 / 待重审"], ["human", "人工控制"], ["cancelled", "已取消"]].forEach(([cls, label]) => {
      const item = el("span", "flow-legend-item"); item.append(el("i", `flow-legend-dot is-${cls}`), el("span", "", label)); legend.append(item);
    });
    section.append(header, scroller, note, legend);
    return section;
  }

  function gatePassedLabel(w) {
    if (!w.check_sha || !w.head_sha) return "尚未确认";
    if (w.check_sha.toLowerCase() !== w.head_sha.toLowerCase()) return "旧提交";
    const remotePassed = w.control === "ACTIVE" && String(w.gate_status).toLowerCase() === "completed" && String(w.gate_conclusion).toLowerCase() === "success";
    if (remotePassed && flowReviewEligibleForGate(w)) return "成功";
    if (remotePassed && w.review?.verdict === "OBSOLETE") return "远端通过 · 审查已过期";
    if (remotePassed) return "远端通过 · 本地审查未确认";
    return gateLabel(w.gate_status, w.gate_conclusion);
  }

  function flowReviewEligibleForGate(w) {
    return w.review?.verdict === "PASS" && w.review?.verification_passed === true && w.review?.skeptic_complete === true &&
      ["READY", "AWAITING_MAINTAINER"].includes(w.phase);
  }

  function reviewMilestoneLabel(w, node) {
    const obsolete = w.review?.verdict === "OBSOLETE";
    if (node === "verify") {
      if (!w.review?.verification_passed) return "等待验证";
      return obsolete ? "历史 · 待重审" : "已通过";
    }
    if (node === "skeptic") {
      if (!w.review?.skeptic_complete) return "待反证";
      return obsolete ? "历史 · 待重审" : "已完成";
    }
    const task = w.review?.tasks?.[node === "reviewer1" ? 0 : 1];
    if (!task) return "待分配";
    if (obsolete && task.status === "ACCEPTED") return "历史 · 待重审";
    return taskLabel(task.status);
  }

  function flowCurrentLabel(w) {
    const phase = w.phase;
    if (w.review?.verdict === "OBSOLETE") return "审查待重做";
    if (phase === "REVIEWING") {
      const tasks = w.review?.tasks || [];
      if (tasks.some(task => task.status !== "ACCEPTED")) return "Reviewer 审查";
      if (!w.review?.skeptic_complete) return "Skeptic 反证";
      return "等待门禁确认";
    }
    return ({ INTAKE: "入队资格", WAITING_CONFIGURATION: "等待授权 / 配置", TRIAGE: "需求确认", NEEDS_INFO: "等待需求补充",
      SPEC_READY: "批准规格", PLANNING: "开发计划", DEVELOPING: "开发任务", CHANGES_REQUESTED: "修复 / 反证",
      VERIFYING: "可信验证", READY: "远端门禁", AWAITING_MAINTAINER: "待维护者合并", MERGED: "已合并",
      BLOCKED: "受阻", DEFERRED: "已暂缓", REJECT_PROPOSED: "维护者覆核", REJECTED: "已拒绝" })[phase] || phaseLabel(phase);
  }

  function flowStateNote(w) {
    const control = { PAUSED: "该工作流已暂停。", PAUSING: "该工作流正在暂停。", BLOCKED: "该工作流受阻，未完成步骤保持待执行或受阻状态。", CANCELLING: "该工作流正在取消。", CANCELLED: "该工作流已取消，后续步骤没有被标记为完成。", HUMAN_OWNED: "该工作流由维护者接管。" }[w.control];
    const phase = { NEEDS_INFO: "当前等待需求补充；收到回复后可回到需求确认。", DEFERRED: "该请求已暂缓，未批准的后续步骤仍为待执行。", REJECTED: "该请求已被拒绝，后续步骤保持待执行。", REJECT_PROPOSED: "拒绝或延期建议等待维护者覆核。", CHANGES_REQUESTED: "验证或审查要求修改；图中显示修复回路。", BLOCKED: "工作流受阻；图中未完成的步骤不会被标为成功。" }[w.phase];
    const gateCurrent = w.check_sha && w.head_sha && w.check_sha.toLowerCase() === w.head_sha.toLowerCase();
    const review = w.review?.verdict === "OBSOLETE" ? "当前 Reviewer / Skeptic 结果已过期，显示为历史并待重审。" : "";
    const gate = w.check_sha && !gateCurrent
      ? "当前门禁对应旧提交，当前 head 尚未确认。"
      : "门禁只有在当前 head 的远端 Check Run 成功、当前审查为 PASS、可信验证与 Skeptic 均完成且处于 READY / 待维护者合并时才显示成功。";
    return [control, phase, review, gate].filter(Boolean).join(" ");
  }

  function renderWorkflowDetail(w, tab = "overview") {
    const container = $("#workflow-detail");
    if (!w) {
      delete container.dataset.renderSignature;
      container.replaceChildren(el("div", "detail-placeholder", "工作流尚未加载或已不存在。"));
      return;
    }
    const signature = `${w.id}|${tab}|${JSON.stringify(w)}`;
    if (container.dataset.renderSignature === signature) return;
    const scrollY = window.scrollY;
    const oldContent = $("#workflow-detail-content", container);
    const contentScrollTop = oldContent?.scrollTop || 0;
    container.dataset.renderSignature = signature;
    container.replaceChildren();

    const heading = el("div", "detail-heading");
    const intro = el("div");
    intro.append(el("h1", "", workflowTitle(w)), el("p", "", `${text(w.repository)} · Issue #${text(w.issue)} · ${text(w.id)}`));
    const meta = el("div", "detail-meta");
    meta.append(statusBadge(w.phase, phaseLabel(w.phase)), statusBadge(w.control, controlLabel(w.control)), statusBadge("muted", `代次 ${text(w.generation, 0)}`));
    if (w.pr) meta.append(statusBadge("blue", `PR #${w.pr}`));
    intro.append(meta);
    const actions = el("div", "command-actions");
    const phaseCanAccept = ["INTAKE", "SPEC_READY"].includes(w.phase) && !["CANCELLING", "CANCELLED"].includes(w.control);
    const phaseCanRejectOrDefer = ["INTAKE", "TRIAGE", "SPEC_READY"].includes(w.phase);
    const phaseCanPause = w.control === "ACTIVE" && !["MERGED", "BLOCKED"].includes(w.phase);
    const phaseCanResume = w.phase === "BLOCKED" ? !["COMPLETED", "CANCELLED", "CANCELLING"].includes(w.control) : !["ACTIVE", "COMPLETED", "CANCELLED", "CANCELLING"].includes(w.control);
    for (const command of ["accept", "resume", "pause"]) {
      const button = el("button", "command-button", commandNames[command]);
      button.type = "button";
      button.dataset.command = command;
      button.disabled = command === "accept" ? !phaseCanAccept : command === "resume" ? !phaseCanResume : !phaseCanPause;
      if (button.disabled) continue;
      button.addEventListener("click", () => sendCommand(w, command));
      actions.append(button);
    }
    const more = el("details", "command-menu");
    const summary = el("summary", "", "更多操作");
    more.append(summary);
    const menu = el("div", "command-menu-items");
    for (const command of ["status", "replan", "review deep", "takeover", "reject", "defer", "cancel"]) {
      const button = el("button", "command-button", commandNames[command]);
      button.type = "button";
      button.dataset.command = command;
      if (command === "reject") button.disabled = w.phase !== "SPEC_READY";
      if (command === "defer") button.disabled = !phaseCanRejectOrDefer;
      if (command === "review deep") button.disabled = !w.pr || w.control === "COMPLETED";
      if (command === "replan") button.disabled = w.phase === "MERGED" || ["CANCELLED", "CANCELLING"].includes(w.control);
      if (button.disabled) button.title = "当前阶段或控制状态不支持此操作";
      button.addEventListener("click", () => sendCommand(w, command));
      menu.append(button);
    }
    more.append(menu);
    actions.append(more);
    heading.append(intro, actions);

    const tabs = el("nav", "detail-tabs");
    const tabNames = { overview: "概况", development: "开发任务", review: "审查与发现", evidence: "证据" };
    for (const [key, label] of Object.entries(tabNames)) {
      const link = el("a", key === tab ? "active" : "", label);
      link.href = `#/workflows/${encodeURIComponent(w.id)}/${key}`;
      link.dataset.detailTab = key;
      tabs.append(link);
    }
    const body = el("div", "detail-content");
    body.id = "workflow-detail-content";

    const grid = el("div", "detail-grid");
    if (tab === "overview") {
      body.append(renderWorkflowDiagram(w));
      const metrics = el("ul", "detail-list");
      detailPair(metrics, "模型调用", w.model_calls);
      detailPair(metrics, "输入 / 输出 Token", `${formatNumber(w.input_tokens)} / ${formatNumber(w.output_tokens)}`);
      detailPair(metrics, "运行时长", `${formatNumber(w.runtime_seconds)} 秒`);
      detailPair(metrics, "修复轮次", w.fix_rounds);
      detailPair(metrics, "PR / 分支", [w.pr ? `#${w.pr}` : "无 PR", w.branch].filter(Boolean).join(" · "));
      detailPair(metrics, "Base / Head", [w.base_sha, w.head_sha].filter(Boolean).join(" / "));
      const gateIsForCurrentHead = !w.check_sha || w.check_sha.toLowerCase() === String(w.head_sha || "").toLowerCase();
      detailPair(metrics, "GitHub 门禁", gateIsForCurrentHead ? gateLabel(w.gate_status, w.gate_conclusion) : "旧提交的门禁，当前提交尚未确认");
      detailPair(metrics, "Check Run", [w.check_id ? `#${w.check_id}` : "", w.check_sha || ""].filter(Boolean).join(" · ") || "尚无 Check Run");
      if (w.gate_confirmed_at && new Date(w.gate_confirmed_at).getTime() > 0) detailPair(metrics, "门禁最后确认", formatTime(w.gate_confirmed_at));
      detailPair(metrics, "更新时间", formatTime(w.updated_at));
      grid.append(detailSection("运行指标", null, metrics));
      const spec = el("div", "proposal-content");
      spec.append(el("h4", "", text(w.spec?.title, "当前规格")), el("p", "", text(w.spec?.description, "暂无规格说明")), el("strong", "subsection-title", "验收标准"), criteriaList(w.spec));
      grid.append(detailSection(w.spec?.approved ? "已批准规格" : "当前规格（待批准）", w.spec?.criteria?.length || 0, spec));
      if (w.last_error) grid.append(detailSection("最近错误", null, el("div", "error-box", w.last_error), true));
    } else if (tab === "development") {
      const tasks = el("div");
      if (Array.isArray(w.tasks) && w.tasks.length) w.tasks.forEach(task => {
        const item = el("div", "item-card");
        item.append(el("strong", "", `${text(task.id, "任务")} · ${taskLabel(task.status)}`));
        item.append(el("p", "", `类型：${({code:"代码",verification:"验证",analysis:"分析"})[task.kind || "code"] || text(task.kind, "代码")} · ${text(task.description, "无任务描述")}`));
        if (task.paths?.length) item.append(el("p", "", `文件：${task.paths.join("、")}`));
        item.append(el("p", "", `尝试 ${number(task.attempts)} 次${task.integrated_sha ? ` · Commit ${task.integrated_sha}` : ""}`));
        if (task.report) item.append(el("pre", "inline-report", typeof task.report === "string" ? task.report : JSON.stringify(task.report, null, 2)));
        tasks.append(item);
      }); else tasks.append(el("p", "muted", "暂无开发任务记录。"));
      grid.append(detailSection("开发任务", w.tasks?.length || 0, tasks, true));
    } else if (tab === "review") {
      grid.append(detailSection("审查与发现", (w.review?.findings?.length || 0) + (w.review?.tasks?.length || 0), reviewContent(w), true));
    } else if (tab === "evidence") {
      const evidence = el("div");
      renderEvidenceTable(w, evidence);
      grid.append(detailSection("验证证据", Array.isArray(w.evidence) ? w.evidence.length : 0, evidence, true));
    }
    body.append(grid);
    container.append(heading, tabs, body);
    window.requestAnimationFrame(() => {
      window.scrollTo({ top: scrollY, behavior: "auto" });
      body.scrollTop = contentScrollTop;
    });
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
    populateFilters(state.workflows);
    if (state.route?.page === "workflows") renderWorkflows(state.workflows);
    if (state.route?.page === "workflow-detail") {
      renderWorkflowDetail(state.workflows.find(w => w.id === state.route.id), state.route.tab);
    }
    if (state.route?.page === "settings" && state.route.tab === "github") renderGitHubIdentifiers(state.config);
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
    const signature = JSON.stringify({ github, repos });
    if (signature === githubInfoSignature) return;
    githubInfoSignature = signature;
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
      const select = $("#installation-select");
      select.replaceChildren(new Option(installations.length ? "请选择安装" : "未发现安装", ""));
      installations.forEach(installation => {
        const account = installation.account?.login || "GitHub 账户";
        select.add(new Option(`${account} · Installation ${installation.id} · App ${installation.app_id}`, installation.id));
      });
      $("#bind-installation").disabled = installations.length === 0;
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

  const workflowTabs = new Set(["overview", "development", "review", "evidence"]);
  const settingsTabs = new Set(["model", "budget", "github"]);
  const routeTitles = { dashboard: "仪表盘", workflows: "工作流列表", "workflow-detail": "工作流详情", events: "事件审计", settings: "设置" };

  function parseRoute() {
    const raw = location.hash.replace(/^#\/?/, "");
    const parts = raw.split("/").filter(Boolean).map(part => {
      try { return decodeURIComponent(part); } catch { return part; }
    });
    if (parts[0] === "workflows" && parts[1] && parts[1] !== "list") {
      return { page: "workflow-detail", id: parts[1], tab: workflowTabs.has(parts[2]) ? parts[2] : "overview" };
    }
    if (parts[0] === "workflows") return { page: "workflows" };
    if (parts[0] === "events") return { page: "events" };
    if (parts[0] === "settings") return { page: "settings", tab: settingsTabs.has(parts[1]) ? parts[1] : "model" };
    return { page: "dashboard" };
  }

  function navigate(path) {
    const next = `#${path}`;
    if (location.hash === next) applyRoute(true);
    else location.hash = next;
  }

  function renderSettingsTab(tab) {
    for (const pane of $$("[data-settings-panel]")) pane.hidden = pane.dataset.settingsPanel !== tab;
    for (const link of $$("[data-settings-route]")) {
      const active = link.dataset.settingsRoute === tab;
      link.classList.toggle("active", active);
      if (active) link.setAttribute("aria-current", "page"); else link.removeAttribute("aria-current");
    }
  }

  function setNavGroup(name, expanded) {
    const group = $(`#${name}-nav-group`);
    const toggle = $(`#${name}-nav-toggle`);
    if (!group || !toggle) return;
    group.classList.toggle("collapsed", !expanded);
    toggle.setAttribute("aria-expanded", String(expanded));
  }

  function renderNavigation(route) {
    const activeRoute = route.page === "dashboard" ? "/dashboard"
      : route.page === "workflows" ? "/workflows/list"
        : route.page === "events" ? "/events"
          : route.page === "settings" ? `/settings/${route.tab}` : "";
    for (const link of $$("[data-route]")) {
      const active = link.dataset.route === activeRoute;
      link.classList.toggle("active", active);
      if (active) link.setAttribute("aria-current", "page"); else link.removeAttribute("aria-current");
    }
    const isWorkflow = route.page === "workflows" || route.page === "workflow-detail";
    $("#workflow-nav-toggle")?.classList.toggle("active", isWorkflow);
    $("#settings-nav-toggle")?.classList.toggle("active", route.page === "settings");
    if (isWorkflow) setNavGroup("workflow", true);
    if (route.page === "settings") setNavGroup("settings", true);
  }

  function applyRoute(scroll = false) {
    const previous = state.route;
    const route = parseRoute();
    state.route = route;
    if (route.page === "workflow-detail") state.selected = route.id;
    for (const panel of $$(".view-panel")) panel.hidden = panel.dataset.panel !== route.page;
    const workflowTabNames = { overview: "概况", development: "开发任务", review: "审查与发现", evidence: "证据" };
    $("#page-title").textContent = route.page === "workflow-detail"
      ? `${routeTitles[route.page]} · ${workflowTabNames[route.tab]}` : routeTitles[route.page] || "仪表盘";
    $("#detail-route-label").textContent = route.page === "workflow-detail"
      ? ({ overview: "工作流概况", development: "开发任务", review: "审查与发现", evidence: "验证证据" })[route.tab] : "工作流详情";
    renderNavigation(route);
    if (route.page === "workflows") renderWorkflows(state.workflows);
    if (route.page === "workflow-detail") renderWorkflowDetail(state.workflows.find(w => w.id === route.id), route.tab);
    if (route.page === "settings") {
      renderSettingsTab(route.tab);
      if (!state.settingsLoaded) loadSettings();
      if (route.tab === "github") renderGitHubIdentifiers(state.config);
    }
    if (route.page === "events" && previous?.page !== "events") loadAudit();
    if (scroll && (previous?.page !== route.page || previous?.id !== route.id || previous?.tab !== route.tab)) {
      window.scrollTo({ top: 0, behavior: "auto" });
    }
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
    $("#workflow-nav-toggle").addEventListener("click", () => setNavGroup("workflow", $("#workflow-nav-group").classList.contains("collapsed")));
    $("#settings-nav-toggle").addEventListener("click", () => setNavGroup("settings", $("#settings-nav-group").classList.contains("collapsed")));
    document.addEventListener("click", event => {
      const link = event.target.closest("a[href]");
      if (!link) return;
      const url = new URL(link.href, location.href);
      if (url.origin !== location.origin || !url.hash.startsWith("#/")) return;
      event.preventDefault();
      navigate(url.hash.slice(1));
    });
    window.addEventListener("hashchange", () => applyRoute(true));
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
  state.route = parseRoute();
  applyRoute(false);
  showLogin();
  refreshState().then(() => {
    if (state.authenticated) startPolling();
  });
})();
