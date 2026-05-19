// disruptoor.js — small UI glue that talks to /v1/* on the same origin.
//
// All mutations go through fetch() against the v1 API so the audit/event
// log captures them the same way as direct curl calls. The UI itself is
// stateless: every successful mutation triggers a page reload.

(function () {
    "use strict";

    function flash(type, message) {
        const wrap = document.getElementById("flash-area");
        if (!wrap) {
            console.log("[disruptoor] " + type + ": " + message);
            return;
        }
        const el = document.createElement("div");
        el.className = "alert alert-" + type + " alert-dismissible fade show";
        el.setAttribute("role", "alert");
        el.appendChild(document.createTextNode(typeof message === "string" ? message : String(message)));
        const close = document.createElement("button");
        close.type = "button";
        close.className = "btn-close";
        close.setAttribute("data-bs-dismiss", "alert");
        close.setAttribute("aria-label", "Close");
        el.appendChild(close);
        wrap.appendChild(el);
    }

    async function readError(res) {
        try {
            const body = await res.json();
            if (body && body.error) {
                return body.error;
            }
            return JSON.stringify(body);
        } catch (_) {
            return res.statusText || ("HTTP " + res.status);
        }
    }

    async function fetchState() {
        const res = await fetch("/v1/state");
        if (!res.ok) {
            throw new Error(await readError(res));
        }
        return {
            state: await res.json(),
            etag: res.headers.get("ETag") || "",
        };
    }

    async function applyState(state, etag) {
        const headers = { "Content-Type": "application/json" };
        if (etag) {
            headers["If-Match"] = etag;
        }
        const res = await fetch("/v1/state", {
            method: "PUT",
            headers: headers,
            body: JSON.stringify(state),
        });
        if (!res.ok) {
            if (res.status === 412) {
                throw new Error("State changed since it was loaded. Reload and try again.");
            }
            throw new Error(await readError(res));
        }
        return await res.json();
    }

    async function clearState() {
        const res = await fetch("/v1/state/clear", { method: "POST" });
        if (!res.ok) {
            throw new Error(await readError(res));
        }
        return await res.json();
    }

    function reloadPage() {
        const y = window.scrollY;
        sessionStorage.setItem("disruptoor.scroll", String(y));
        location.reload();
    }

    // ---- Discovery (containers + labels) -----------------------------------

    // Kurtosis label used by ethereum-package to record the per-service
    // human-readable id. Matches against this key are unambiguous (vs the
    // shorter "id" form which gets prefixed with the custom-namespace).
    const CONTAINER_ID_LABEL = "com.kurtosistech.id";

    let discoveryPromise = null;
    function fetchDiscovery() {
        if (discoveryPromise) {
            return discoveryPromise;
        }
        discoveryPromise = (async () => {
            const res = await fetch("/webui/api/containers");
            if (!res.ok) {
                discoveryPromise = null;
                throw new Error(await readError(res));
            }
            const body = await res.json();
            const containers = Array.isArray(body.containers) ? body.containers : [];
            const names = [];
            const labelKeys = new Set();
            for (const c of containers) {
                const name = c.Name || c.name || "";
                if (name) names.push(name);
                const labels = c.Labels || c.labels || {};
                for (const k of Object.keys(labels)) {
                    labelKeys.add(k);
                }
            }
            names.sort();
            return {
                containers: names,
                labelKeys: Array.from(labelKeys).sort(),
            };
        })();
        return discoveryPromise;
    }

    // Populate the shared <datalist id="discovered-label-keys"> with both
    // fully-qualified label keys and short forms (everything after the
    // ethereum-package custom prefix).
    function fillLabelKeyDatalist(labelKeys) {
        const dl = document.getElementById("discovered-label-keys");
        if (!dl) return;
        const prefix = "com.kurtosistech.custom.ethereum-package.";
        const seen = new Set();
        const add = (v) => {
            if (!v || seen.has(v)) return;
            seen.add(v);
            const opt = document.createElement("option");
            opt.value = v;
            dl.appendChild(opt);
        };
        dl.innerHTML = "";
        for (const k of labelKeys) {
            add(k);
            if (k.startsWith(prefix)) {
                add(k.slice(prefix.length));
            }
        }
    }

    // ---- Selector builder --------------------------------------------------
    //
    // A "selector builder" is a card cloned from the page template. It owns a
    // mode dropdown (all / containers / label) and three sibling panels, one
    // per mode. Public API:
    //   builder.serialize()  -> {ok: true, value: <Selector>} | {ok: false, error: <string>}
    //   builder.isEmpty()    -> boolean (true if user hasn't picked anything)
    //
    // Selector shape on the wire (matches internal/state.Selector.MarshalJSON):
    //   "all"
    //   {labelKey: ["v1", "v2"]}

    let builderUid = 0;

    function decorateBuilder(card, opts) {
        opts = opts || {};
        const uid = ++builderUid;
        const modeSel = card.querySelector('[data-role="mode"]');
        const panels = {
            all: card.querySelector('[data-role="mode-panel-all"]'),
            containers: card.querySelector('[data-role="mode-panel-containers"]'),
            label: card.querySelector('[data-role="mode-panel-label"]'),
        };
        const picker = card.querySelector('[data-role="container-picker"]');
        const filterInput = card.querySelector('[data-role="container-filter"]');
        const selectAllBtn = card.querySelector('[data-role="picker-all"]');
        const selectNoneBtn = card.querySelector('[data-role="picker-none"]');
        const containerHelp = card.querySelector('[data-role="container-help"]');
        const labelKey = card.querySelector('[data-role="label-key"]');
        const labelVals = card.querySelector('[data-role="label-values"]');
        const removeBtn = card.querySelector('[data-action="remove-selector"]');
        const title = card.querySelector(".selector-builder-title");
        if (title && opts.title) title.textContent = opts.title;

        function showMode(mode) {
            for (const m of Object.keys(panels)) {
                if (!panels[m]) continue;
                panels[m].classList.toggle("d-none", m !== mode);
            }
        }
        modeSel.addEventListener("change", () => showMode(modeSel.value));
        showMode(modeSel.value);

        if (removeBtn) {
            if (opts.removable === false) {
                removeBtn.classList.add("d-none");
            } else {
                removeBtn.addEventListener("click", () => {
                    card.remove();
                    if (typeof opts.onRemove === "function") opts.onRemove();
                });
            }
        }

        function pickerChecks() {
            return picker ? Array.from(picker.querySelectorAll('input[type="checkbox"]')) : [];
        }

        function applyFilter() {
            const q = (filterInput && filterInput.value ? filterInput.value : "").trim().toLowerCase();
            for (const cb of pickerChecks()) {
                const row = cb.closest(".container-picker-item");
                if (!row) continue;
                const match = !q || cb.value.toLowerCase().includes(q);
                row.classList.toggle("d-none", !match);
            }
        }

        if (filterInput) {
            filterInput.addEventListener("input", applyFilter);
        }
        if (selectAllBtn) {
            selectAllBtn.addEventListener("click", () => {
                for (const cb of pickerChecks()) {
                    const row = cb.closest(".container-picker-item");
                    if (row && row.classList.contains("d-none")) continue;
                    cb.checked = true;
                }
            });
        }
        if (selectNoneBtn) {
            selectNoneBtn.addEventListener("click", () => {
                for (const cb of pickerChecks()) cb.checked = false;
            });
        }

        // Populate the container picker once discovery completes. We do it
        // here rather than up-front so the form is usable even before the
        // request lands.
        fetchDiscovery().then((d) => {
            if (picker) {
                picker.innerHTML = "";
                if (d.containers.length === 0) {
                    picker.classList.add("is-empty");
                    if (filterInput) filterInput.disabled = true;
                    if (selectAllBtn) selectAllBtn.disabled = true;
                    if (selectNoneBtn) selectNoneBtn.disabled = true;
                    const empty = document.createElement("div");
                    empty.className = "container-picker-empty text-muted small";
                    empty.textContent = "No containers discovered.";
                    picker.appendChild(empty);
                    if (containerHelp) {
                        containerHelp.textContent =
                            "No containers discovered. Use Label match or check the Containers page.";
                        containerHelp.classList.add("text-warning");
                    }
                } else {
                    picker.classList.remove("is-empty");
                    d.containers.forEach((name, idx) => {
                        const row = document.createElement("label");
                        row.className = "container-picker-item";
                        const cb = document.createElement("input");
                        cb.type = "checkbox";
                        cb.className = "form-check-input flex-shrink-0";
                        cb.value = name;
                        cb.id = "container-pick-" + uid + "-" + idx;
                        const text = document.createElement("span");
                        text.className = "container-picker-name font-monospace small";
                        text.textContent = name;
                        row.appendChild(cb);
                        row.appendChild(text);
                        picker.appendChild(row);
                    });
                    applyFilter();
                }
            }
            fillLabelKeyDatalist(d.labelKeys);
        }).catch((err) => {
            if (containerHelp) {
                containerHelp.textContent = "Container discovery failed: " + err.message;
                containerHelp.classList.add("text-danger");
            }
            if (filterInput) filterInput.disabled = true;
            if (selectAllBtn) selectAllBtn.disabled = true;
            if (selectNoneBtn) selectNoneBtn.disabled = true;
        });

        function selectedContainers() {
            return pickerChecks().filter((cb) => cb.checked).map((cb) => cb.value);
        }

        function isEmpty() {
            const mode = modeSel.value;
            if (mode === "all") return false;
            if (mode === "containers") {
                return selectedContainers().length === 0;
            }
            if (mode === "label") {
                return (
                    (!labelKey.value || !labelKey.value.trim()) &&
                    (!labelVals.value || !labelVals.value.trim())
                );
            }
            return true;
        }

        function serialize() {
            const mode = modeSel.value;
            if (mode === "all") {
                return { ok: true, value: "all" };
            }
            if (mode === "containers") {
                const values = selectedContainers();
                if (values.length === 0) {
                    return { ok: false, error: "tick at least one container" };
                }
                return { ok: true, value: { [CONTAINER_ID_LABEL]: values } };
            }
            if (mode === "label") {
                const key = (labelKey.value || "").trim();
                const raw = (labelVals.value || "").trim();
                if (!key) return { ok: false, error: "label key required" };
                if (!raw) return { ok: false, error: "label values required" };
                const values = raw
                    .split(",")
                    .map((v) => v.trim())
                    .filter(Boolean);
                if (values.length === 0) {
                    return { ok: false, error: "label values required" };
                }
                return { ok: true, value: { [key]: values } };
            }
            return { ok: false, error: "unknown mode" };
        }

        return { card, serialize, isEmpty };
    }

    function newBuilder(opts) {
        const tpl = document.getElementById("selector-builder-template");
        if (!tpl) return null;
        const node = tpl.content.firstElementChild.cloneNode(true);
        return decorateBuilder(node, opts);
    }

    // ---- Partition modal ---------------------------------------------------

    function initPartitionModal() {
        const modal = document.getElementById("addPartitionModal");
        const form = document.getElementById("partition-add-form");
        const list = document.getElementById("partition-groups");
        const warning = document.getElementById("partition-discovery-warning");
        const addBtn = document.querySelector('[data-action="add-partition-group"]');
        if (!modal || !form || !list || !addBtn) return;

        const builders = [];

        function renumber() {
            builders.forEach((b, idx) => {
                const t = b.card.querySelector(".selector-builder-title");
                if (t) t.textContent = "Group " + (idx + 1);
            });
        }

        function addGroup() {
            const b = newBuilder({
                title: "Group " + (builders.length + 1),
                removable: true,
                onRemove: () => {
                    const i = builders.indexOf(b);
                    if (i >= 0) builders.splice(i, 1);
                    renumber();
                },
            });
            if (!b) return;
            builders.push(b);
            list.appendChild(b.card);
            renumber();
        }

        addBtn.addEventListener("click", addGroup);

        modal.addEventListener("show.bs.modal", () => {
            // Reset to a fresh form every open.
            form.reset();
            list.innerHTML = "";
            builders.length = 0;
            addGroup();
            addGroup();

            // Surface discovery problems near the groups.
            warning.classList.add("d-none");
            warning.textContent = "";
            fetchDiscovery()
                .then((d) => {
                    if (d.containers.length === 0) {
                        warning.textContent =
                            "Heads up: discovery returned 0 containers. The 'Specific containers' picker will be empty.";
                        warning.classList.remove("d-none");
                    }
                })
                .catch((err) => {
                    warning.textContent = "Discovery failed: " + err.message;
                    warning.classList.remove("d-none");
                });
        });

        form.addEventListener("submit", async (ev) => {
            ev.preventDefault();
            const name = form.querySelector('[name="name"]').value.trim();
            if (!name) {
                flash("danger", "Partition name required.");
                return;
            }
            if (builders.length < 2) {
                flash("danger", "Need at least 2 groups.");
                return;
            }
            const groups = [];
            for (let i = 0; i < builders.length; i++) {
                const r = builders[i].serialize();
                if (!r.ok) {
                    flash("danger", "Group " + (i + 1) + ": " + r.error);
                    return;
                }
                groups.push(r.value);
            }
            const scope = Array.from(form.querySelectorAll('input[name="scope"]:checked')).map((cb) => cb.value);
            const symmetricVal = form.querySelector('[name="symmetric"]').value;
            const part = { name: name, groups: groups };
            if (scope.length > 0) part.scope = scope;
            if (symmetricVal !== "default") {
                part.symmetric = symmetricVal === "true";
            }
            try {
                const cur = await fetchState();
                const state = cur.state;
                state.partitions = state.partitions || [];
                state.partitions.push(part);
                await applyState(state, cur.etag);
                flash("success", 'Added partition "' + name + '".');
                reloadPage();
            } catch (err) {
                flash("danger", "Add failed: " + err.message);
            }
        });
    }

    // ---- Shaping modal -----------------------------------------------------

    function initShapingModal() {
        const modal = document.getElementById("addShapingModal");
        const form = document.getElementById("shaping-add-form");
        const targetSlot = document.getElementById("shaping-target");
        if (!modal || !form || !targetSlot) return;

        let targetBuilder = null;

        modal.addEventListener("show.bs.modal", () => {
            form.reset();
            targetSlot.innerHTML = "";
            // Re-tick the required scope checkbox; reset() unchecks it even
            // though `checked` is the default attribute.
            const scopeCb = form.querySelector('input[name="scope"][value="include_control"]');
            if (scopeCb) scopeCb.checked = true;
            targetBuilder = newBuilder({ title: "Target", removable: false });
            if (targetBuilder) {
                targetSlot.appendChild(targetBuilder.card);
            }
        });

        form.addEventListener("submit", async (ev) => {
            ev.preventDefault();
            const name = form.querySelector('[name="name"]').value.trim();
            if (!name) {
                flash("danger", "Shaping name required.");
                return;
            }
            if (!targetBuilder) {
                flash("danger", "Target missing.");
                return;
            }
            const tr = targetBuilder.serialize();
            if (!tr.ok) {
                flash("danger", "Target: " + tr.error);
                return;
            }
            const scope = Array.from(form.querySelectorAll('input[name="scope"]:checked')).map((cb) => cb.value);
            const delay = form.querySelector('[name="delay"]').value.trim();
            const jitter = form.querySelector('[name="jitter"]').value.trim();
            const loss = form.querySelector('[name="loss"]').value.trim();
            const bandwidth = form.querySelector('[name="bandwidth"]').value.trim();
            if (!delay && !loss && !bandwidth) {
                flash("danger", "At least one of delay, loss, bandwidth is required.");
                return;
            }
            const sh = { name: name, target: tr.value };
            if (scope.length > 0) sh.scope = scope;
            if (delay) sh.delay = delay;
            if (jitter) sh.jitter = jitter;
            if (loss) sh.loss = loss;
            if (bandwidth) sh.bandwidth = bandwidth;
            try {
                const cur = await fetchState();
                const state = cur.state;
                state.shaping = state.shaping || [];
                state.shaping.push(sh);
                await applyState(state, cur.etag);
                flash("success", 'Added shaping rule "' + name + '".');
                reloadPage();
            } catch (err) {
                flash("danger", "Add failed: " + err.message);
            }
        });
    }

    // ---- Page hookups ------------------------------------------------------

    document.addEventListener("DOMContentLoaded", function () {
        const y = sessionStorage.getItem("disruptoor.scroll");
        if (y) {
            sessionStorage.removeItem("disruptoor.scroll");
            window.scrollTo(0, parseInt(y, 10) || 0);
        }

        document.querySelectorAll('[data-action="clear-all"]').forEach(function (btn) {
            btn.addEventListener("click", async function (ev) {
                ev.preventDefault();
                if (!confirm("Clear all active partitions and shaping rules?")) {
                    return;
                }
                try {
                    await clearState();
                    flash("success", "State cleared.");
                    reloadPage();
                } catch (err) {
                    flash("danger", "Clear failed: " + err.message);
                }
            });
        });

        document.querySelectorAll('[data-action="remove-entry"]').forEach(function (btn) {
            btn.addEventListener("click", async function (ev) {
                ev.preventDefault();
                const kind = btn.getAttribute("data-kind");
                const name = btn.getAttribute("data-name");
                if (!confirm("Remove " + kind + ' "' + name + '"?')) {
                    return;
                }
                try {
                    const cur = await fetchState();
                    const state = cur.state;
                    if (kind === "partition") {
                        state.partitions = (state.partitions || []).filter(p => p.name !== name);
                    } else if (kind === "shaping") {
                        state.shaping = (state.shaping || []).filter(s => s.name !== name);
                    }
                    await applyState(state, cur.etag);
                    flash("success", "Removed " + kind + ' "' + name + '".');
                    reloadPage();
                } catch (err) {
                    flash("danger", "Remove failed: " + err.message);
                }
            });
        });

        const editor = document.getElementById("state-editor");
        const applyBtn = document.getElementById("state-apply-btn");
        if (editor && applyBtn) {
            applyBtn.addEventListener("click", async function () {
                let parsed;
                try {
                    parsed = JSON.parse(editor.value);
                } catch (err) {
                    flash("danger", "Invalid JSON: " + err.message);
                    return;
                }
                try {
                    await applyState(parsed, editor.getAttribute("data-etag") || "");
                    flash("success", "State applied.");
                    setTimeout(reloadPage, 600);
                } catch (err) {
                    flash("danger", "Apply failed: " + err.message);
                }
            });
        }

        initPartitionModal();
        initShapingModal();
    });

    if (typeof ClipboardJS !== "undefined") {
        new ClipboardJS("[data-clipboard-text],[data-clipboard-target]");
    }
})();
