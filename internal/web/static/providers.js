// Provider form: toggle the api-key vs oauth field groups by the selected
// auth method, and prefill the oauth config from a chosen preset's data-*
// attributes. Visibility is driven by the form's data-method attribute so a
// server-rendered edit row starts in the correct state without JS running.

function airouterToggleAuthMethod(sel) {
  const form = sel.closest(".provider-form");
  if (form) {
    form.setAttribute("data-method", sel.value);
  }
}

// airouterToggleOAuthMode drives the form's data-oauth-mode attribute so an
// interactive-oauth form shows either the web-auth (Connect) group or the manual
// token-import group, never both. Mirrors the auth-method toggle; visibility is
// CSS-driven off the attribute. The Grok Build select submits "device" while the
// shared CSS group is still called "web".
function airouterToggleOAuthMode(sel) {
  const form = sel.closest(".provider-form");
  if (form) {
    form.setAttribute("data-oauth-mode", sel.value === "device" ? "web" : sel.value);
    airouterSyncOAuthFields(form);
  }
}

// airouterSyncOAuthFields disables the hidden group on forms that opt in with
// data-oauth-lock. This keeps manual secrets out of Connect/Check requests and
// keeps a stale oauth_session out of a manual import. Server validation still
// rejects a mixed form if JavaScript is unavailable.
function airouterSyncOAuthFields(form) {
  if (!form || !form.hasAttribute("data-oauth-lock")) {
    return;
  }
  const manual = form.getAttribute("data-oauth-mode") === "manual";
  const webControls = form.querySelectorAll(
    ".oauth-web input, .oauth-web select, .oauth-web textarea, .oauth-web button"
  );
  webControls.forEach(function (el) {
    el.disabled = manual;
  });
  const manualControls = form.querySelectorAll(
    ".oauth-manual input, .oauth-manual select, .oauth-manual textarea, .oauth-manual button"
  );
  manualControls.forEach(function (el) {
    el.disabled = !manual;
  });
}

function airouterSyncOpencodeForm(form) {
  if (!form) {
    return;
  }
  const tier = form.querySelector("[name=opencode_tier]");
  const auth = form.querySelector("[name=opencode_auth]");
  if (!tier) {
    return;
  }
  form.setAttribute("data-opencode-tier", tier.value);
  if (tier.value !== "go" && auth) {
    auth.value = "apikey";
  }
  form.setAttribute("data-opencode-auth", auth ? auth.value : "apikey");
}

function airouterToggleOpencodeTier(sel) {
  airouterSyncOpencodeForm(sel.closest(".provider-form"));
}

function airouterToggleOpencodeAuth(sel) {
  airouterSyncOpencodeForm(sel.closest(".provider-form"));
}

// airouterSelectProviderCard marks the clicked card as the active one so the
// grid shows which recipe's add-form is open. Selection is purely visual and
// non-persistent; a page reload clears it, which matches the empty form slot on
// load.
function airouterSelectProviderCard(btn) {
  document.querySelectorAll(".provider-card.selected").forEach(function (c) {
    c.classList.remove("selected");
    c.setAttribute("aria-pressed", "false");
  });
  btn.classList.add("selected");
  btn.setAttribute("aria-pressed", "true");
}

// airouterCloseProviderForm dismisses the open add-form and clears the card
// selection together, so a closed form never leaves a stale-selected card.
function airouterCloseProviderForm() {
  const slot = document.getElementById("provider-form-slot");
  if (slot) {
    slot.innerHTML = "";
  }
  document.querySelectorAll(".provider-card.selected").forEach(function (c) {
    c.classList.remove("selected");
    c.setAttribute("aria-pressed", "false");
  });
}

// Provider Edit/Save/Archive/Restore/Delete all swap either the whole list
// (#provider-list) or a single row (#provider-<id>) with outerHTML, changing the
// document height. When it grows (Edit expands a row into a tall form) or shrinks
// (Save/Archive/Delete), the browser reclamps scroll and can jump the viewport to
// the bottom. Preserve the scroll position across these swaps instead of chasing
// any particular row - scrollIntoView would follow an archived row down into the
// bottom "Archived" section, which is exactly the jump we want to avoid.
let airouterProviderScrollY = null;

function airouterIsProviderSwap(t) {
  return !!t && (t.id === "provider-list" || /^provider-\d+$/.test(t.id));
}

document.body.addEventListener("htmx:beforeSwap", function (event) {
  if (airouterIsProviderSwap(event.detail.target)) {
    airouterProviderScrollY = window.scrollY;
  }
});

// Restore on afterSwap, not afterSettle: afterSwap runs synchronously in the same
// task as the DOM replacement, before the browser paints, so the clamped/jumped
// position never reaches the screen. afterSettle fires on a later timer, letting
// one intermediate frame paint first - the visible flicker.
document.body.addEventListener("htmx:afterSwap", function (event) {
  if (airouterProviderScrollY !== null && airouterIsProviderSwap(event.detail.target)) {
    window.scrollTo({ top: airouterProviderScrollY });
    airouterProviderScrollY = null;
  }
  if (event.detail.target && event.detail.target.id === "provider-list") {
    airouterApplyProviderTagFilter();
  }
  document.querySelectorAll(".provider-form[data-oauth-lock]").forEach(airouterSyncOAuthFields);
});

var airouterProviderTagFilter = "";
var airouterUntaggedFilter = "__untagged__";

function airouterSelectProviderTagFilter(btn) {
  airouterProviderTagFilter = btn.getAttribute("data-tag-filter") || "";
  airouterApplyProviderTagFilter();
}

function airouterRowMatchesTagFilter(row, filter) {
  if (!filter) {
    return true;
  }
  var tags = (row.getAttribute("data-tags") || "").split(",").filter(Boolean);
  if (filter === airouterUntaggedFilter) {
    return tags.length === 0;
  }
  return tags.indexOf(filter) !== -1;
}

function airouterApplyProviderTagFilter() {
  var list = document.getElementById("provider-list");
  if (!list) {
    return;
  }
  var buttons = list.querySelectorAll(".tag-filter-btn");
  var available = {};
  buttons.forEach(function (btn) {
    available[btn.getAttribute("data-tag-filter") || ""] = true;
  });
  if (!available.hasOwnProperty(airouterProviderTagFilter)) {
    airouterProviderTagFilter = "";
  }
  buttons.forEach(function (btn) {
    var on = (btn.getAttribute("data-tag-filter") || "") === airouterProviderTagFilter;
    btn.setAttribute("aria-pressed", on ? "true" : "false");
  });
  var visible = 0;
  list.querySelectorAll("tr[data-tags]").forEach(function (row) {
    var show = airouterRowMatchesTagFilter(row, airouterProviderTagFilter);
    row.hidden = !show;
    if (show) {
      visible += 1;
    }
  });
  list.querySelectorAll("[data-provider-group]").forEach(function (group) {
    var rows = group.querySelectorAll("tr[data-tags]");
    if (rows.length === 0) {
      group.hidden = !!airouterProviderTagFilter;
      return;
    }
    var any = false;
    rows.forEach(function (row) {
      if (!row.hidden) {
        any = true;
      }
    });
    group.hidden = !any;
  });
  var empty = document.getElementById("provider-filter-empty");
  if (empty) {
    empty.hidden = visible !== 0;
  }
}

// airouterResetProviderForm restores the create form to a pristine state after a
// successful add. form.reset() only reverts native input values; the oauth
// connect region, the Check result, and any Refresh status were mutated by HTMX
// swaps and must be cleared back to their initial server-rendered markup by hand.
function airouterResetProviderForm(form) {
  form.reset();
  airouterToggleAuthMethod(form.querySelector(".auth-method"));
  airouterSyncOpencodeForm(form);
  const oauthMode = form.querySelector(".oauth-mode");
  if (oauthMode && form.hasAttribute("data-oauth-lock")) {
    airouterToggleOAuthMode(oauthMode);
  }
  form.querySelectorAll(".oauth-tokens input").forEach(function (i) {
    i.value = "";
  });
  const region = form.querySelector(".oauth-region");
  if (region) {
    region.innerHTML =
      '<span class="muted">Not connected. Fill the fields above and press Connect.</span>';
  }
  form.querySelectorAll(".check-result").forEach(function (el) {
    el.innerHTML = "";
  });
  form.querySelectorAll(".oauth-tokens .check").forEach(function (el) {
    el.remove();
  });
}

// Deferred scripts run after the document is parsed. Apply the initial lock
// state before the first submit.
document.querySelectorAll(".provider-form[data-oauth-lock]").forEach(airouterSyncOAuthFields);
