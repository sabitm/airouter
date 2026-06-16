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
