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
            // page didn't include the flash slot; fall back to console.
            console.log("[disruptoor] " + type + ": " + message);
            return;
        }
        const el = document.createElement("div");
        el.className = "alert alert-" + type + " alert-dismissible fade show";
        el.setAttribute("role", "alert");
        el.innerHTML =
            (typeof message === "string" ? message : String(message)) +
            '<button type="button" class="btn-close" data-bs-dismiss="alert" aria-label="Close"></button>';
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

    async function applyState(state) {
        const res = await fetch("/v1/state", {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(state),
        });
        if (!res.ok) {
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

    // Refresh table-style pages without losing scroll position.
    function reloadPage() {
        const y = window.scrollY;
        sessionStorage.setItem("disruptoor.scroll", String(y));
        location.reload();
    }

    document.addEventListener("DOMContentLoaded", function () {
        const y = sessionStorage.getItem("disruptoor.scroll");
        if (y) {
            sessionStorage.removeItem("disruptoor.scroll");
            window.scrollTo(0, parseInt(y, 10) || 0);
        }

        // Clear-all buttons: any element with data-action="clear-all".
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

        // Remove-one buttons (per partition / per shaping rule). The button
        // carries data-kind="partition|shaping" and data-name="<name>" so we
        // can rebuild the state without that entry.
        document.querySelectorAll('[data-action="remove-entry"]').forEach(function (btn) {
            btn.addEventListener("click", async function (ev) {
                ev.preventDefault();
                const kind = btn.getAttribute("data-kind");
                const name = btn.getAttribute("data-name");
                if (!confirm("Remove " + kind + ' "' + name + '"?')) {
                    return;
                }
                try {
                    const res = await fetch("/v1/state");
                    if (!res.ok) throw new Error(await readError(res));
                    const cur = await res.json();
                    if (kind === "partition") {
                        cur.partitions = (cur.partitions || []).filter(p => p.name !== name);
                    } else if (kind === "shaping") {
                        cur.shaping = (cur.shaping || []).filter(s => s.name !== name);
                    }
                    await applyState(cur);
                    flash("success", "Removed " + kind + ' "' + name + '".');
                    reloadPage();
                } catch (err) {
                    flash("danger", "Remove failed: " + err.message);
                }
            });
        });

        // State editor: textarea + Apply button.
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
                    await applyState(parsed);
                    flash("success", "State applied.");
                    setTimeout(reloadPage, 600);
                } catch (err) {
                    flash("danger", "Apply failed: " + err.message);
                }
            });
        }

        // Partition add form.
        const partForm = document.getElementById("partition-add-form");
        if (partForm) {
            partForm.addEventListener("submit", async function (ev) {
                ev.preventDefault();
                const name = partForm.querySelector('[name="name"]').value.trim();
                const groupsRaw = partForm.querySelector('[name="groups"]').value;
                const scope = partForm.querySelector('[name="scope"]').value.trim();
                const symmetricVal = partForm.querySelector('[name="symmetric"]').value;
                if (!name) {
                    flash("danger", "Partition name required.");
                    return;
                }
                let groups;
                try {
                    groups = JSON.parse(groupsRaw);
                } catch (err) {
                    flash("danger", "Groups field must be JSON array: " + err.message);
                    return;
                }
                if (!Array.isArray(groups) || groups.length < 2) {
                    flash("danger", "Need at least 2 groups.");
                    return;
                }
                const part = { name: name, groups: groups };
                if (scope) {
                    part.scope = scope.split(",").map(s => s.trim()).filter(Boolean);
                }
                if (symmetricVal !== "default") {
                    part.symmetric = symmetricVal === "true";
                }
                try {
                    const res = await fetch("/v1/state");
                    if (!res.ok) throw new Error(await readError(res));
                    const cur = await res.json();
                    cur.partitions = cur.partitions || [];
                    cur.partitions.push(part);
                    await applyState(cur);
                    flash("success", 'Added partition "' + name + '".');
                    reloadPage();
                } catch (err) {
                    flash("danger", "Add failed: " + err.message);
                }
            });
        }

        // Shaping add form.
        const shapeForm = document.getElementById("shaping-add-form");
        if (shapeForm) {
            shapeForm.addEventListener("submit", async function (ev) {
                ev.preventDefault();
                const name = shapeForm.querySelector('[name="name"]').value.trim();
                const targetRaw = shapeForm.querySelector('[name="target"]').value.trim();
                const scope = shapeForm.querySelector('[name="scope"]').value.trim();
                const delay = shapeForm.querySelector('[name="delay"]').value.trim();
                const jitter = shapeForm.querySelector('[name="jitter"]').value.trim();
                const loss = shapeForm.querySelector('[name="loss"]').value.trim();
                const bandwidth = shapeForm.querySelector('[name="bandwidth"]').value.trim();
                if (!name) {
                    flash("danger", "Shaping name required.");
                    return;
                }
                let target;
                try {
                    target = targetRaw === '"all"' || targetRaw === "all"
                        ? "all"
                        : JSON.parse(targetRaw);
                } catch (err) {
                    flash("danger", 'Target must be "all" or JSON object: ' + err.message);
                    return;
                }
                const sh = { name: name, target: target };
                if (scope) {
                    sh.scope = scope.split(",").map(s => s.trim()).filter(Boolean);
                }
                if (delay) sh.delay = delay;
                if (jitter) sh.jitter = jitter;
                if (loss) sh.loss = loss;
                if (bandwidth) sh.bandwidth = bandwidth;
                try {
                    const res = await fetch("/v1/state");
                    if (!res.ok) throw new Error(await readError(res));
                    const cur = await res.json();
                    cur.shaping = cur.shaping || [];
                    cur.shaping.push(sh);
                    await applyState(cur);
                    flash("success", 'Added shaping rule "' + name + '".');
                    reloadPage();
                } catch (err) {
                    flash("danger", "Add failed: " + err.message);
                }
            });
        }
    });

    // Wire up clipboard.js for any [data-clipboard-text] / [data-clipboard-target]
    // buttons. Same convention as spamoor.
    if (typeof ClipboardJS !== "undefined") {
        new ClipboardJS("[data-clipboard-text],[data-clipboard-target]");
    }
})();
