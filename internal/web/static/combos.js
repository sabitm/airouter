// Combo target rows: clone a hidden <template> row, give it a unique index so
// its datalist id and htmx model-fetch wiring stay distinct, and let htmx
// process the new node (its provider select fires the "load" fetch). Removal
// keeps at least one row so a combo always has a target.
function addComboTarget(btn) {
  const form = btn.closest("form");
  const tmpl = form.querySelector(".target-template");
  const container = form.querySelector(".targets");
  window.__comboRow = (window.__comboRow || 0) + 1;
  const idx = "r" + window.__comboRow;
  const holder = document.createElement("div");
  holder.innerHTML = tmpl.innerHTML.replaceAll("__IDX__", idx).trim();
  const row = holder.firstElementChild;
  container.appendChild(row);
  if (window.htmx) {
    window.htmx.process(row);
  }
}

function removeComboTarget(btn) {
  const row = btn.closest(".target-row");
  const container = row.parentElement;
  if (container.querySelectorAll(".target-row").length > 1) {
    row.remove();
  }
}

// Checkbox has no name; the hidden input is what the form submits so unchecked
// rows still post "0" instead of disappearing from the parallel enabled array.
function airouterSetTargetEnabled(cb) {
  const row = cb.closest(".target-row");
  if (!row) return;
  const hidden = row.querySelector('input[name="enabled"]');
  if (hidden) {
    hidden.value = cb.checked ? "1" : "0";
  }
}

// dir -1 moves earlier in failover order (toward top); +1 later (toward bottom).
function moveComboTarget(btn, dir) {
  const row = btn.closest(".target-row");
  if (!row) return;
  const container = row.parentElement;
  if (dir < 0) {
    const prev = row.previousElementSibling;
    if (prev && prev.classList.contains("target-row")) {
      container.insertBefore(row, prev);
    }
    return;
  }
  const next = row.nextElementSibling;
  if (next && next.classList.contains("target-row")) {
    container.insertBefore(next, row);
  }
}

// Combo Edit/Save/Delete/toggle swap #combo-list or a single #combo-<id> row,
// changing document height. Preserve scrollY across those swaps (same approach
// as providers.js) so the viewport does not jump to the bottom.
let airouterComboScrollY = null;

function airouterIsComboSwap(t) {
  return !!t && (t.id === "combo-list" || /^combo-\d+$/.test(t.id));
}

document.body.addEventListener("htmx:beforeSwap", function (event) {
  if (airouterIsComboSwap(event.detail.target)) {
    airouterComboScrollY = window.scrollY;
  }
});

document.body.addEventListener("htmx:afterSwap", function (event) {
  if (airouterComboScrollY !== null && airouterIsComboSwap(event.detail.target)) {
    window.scrollTo({ top: airouterComboScrollY });
    airouterComboScrollY = null;
  }
});
