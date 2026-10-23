// Links for signed-in members, in the menus of a public page.
//
// The public build shows a members-only row (a member page, or a service such
// as the gallery) as a grey label, since the public tree may not name those
// addresses. A member reading a public page is still signed in, though, so
// this asks the gate: it fetches the member section's landing page, the same
// door the sign-in button names. A reader without a session gets a redirect to
// Keycloak and the labels stay grey; a member gets the member build of that
// page, whose menus hold the same rows as links.
//
// Same idiom as mega-menu.js: an IIFE, var, no modules.
(function () {
  var header = document.getElementById("site-header");
  if (!header) return;

  // The answer is kept for the tab, so a reader who is not signed in asks once
  // rather than on every page. Signing in lands on a member page, which resets
  // it, so a stale "no" lasts only until the next login.
  var KEY = "prodeko-signed-in";
  function remember(value) {
    try { sessionStorage.setItem(KEY, value); } catch (e) {}
  }
  function recall() {
    try { return sessionStorage.getItem(KEY); } catch (e) { return null; }
  }

  if (header.hasAttribute("data-member-tree")) {
    remember("1");
    return;
  }

  var probe = header.getAttribute("data-signed-in-probe");
  if (!probe || recall() === "0") return;

  // A members-only row is a grey label in the public build, named by
  // data-members and nothing else, so the page holds no member address. The
  // member build's header has the same row as a link: the mega menu's for a
  // mega-menu row, the phone menu's for the others, whose plain link is also
  // the sidebar's.
  function upgrade(theirs) {
    var links = { mega: {}, plain: {} };
    theirs.querySelectorAll(".mega-panel a[data-members]").forEach(function (a) {
      links.mega[a.getAttribute("data-members")] = a;
    });
    theirs.querySelectorAll("#mobile-nav a[data-members]").forEach(function (a) {
      links.plain[a.getAttribute("data-members")] = a;
    });
    document.querySelectorAll("span[data-members]").forEach(function (span) {
      var kind = span.classList.contains("mega-link-disabled") ? "mega" : "plain";
      var link = links[kind][span.getAttribute("data-members")];
      if (!link) return;
      var copy = document.importNode(link, true);
      copy.removeAttribute("aria-current");
      span.replaceWith(copy);
    });
  }

  // redirect: "manual" keeps the Keycloak redirect from being followed, so a
  // signed-out reader costs one small response. A login page answered with a
  // 200 has no member menu in it and counts as signed out as well.
  fetch(probe, { credentials: "same-origin", redirect: "manual" })
    .then(function (r) {
      var ct = r.headers.get("content-type") || "";
      if (!r.ok || ct.indexOf("html") === -1) return "";
      return r.text();
    })
    .then(function (html) {
      var doc = html ? new DOMParser().parseFromString(html, "text/html") : null;
      var theirs = doc && doc.getElementById("site-header");
      if (!theirs || !theirs.querySelector("a[data-members]")) {
        remember("0");
        return;
      }
      remember("1");
      upgrade(theirs);
    })
    .catch(function () {});
})();
