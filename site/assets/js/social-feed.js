/* Click-to-load social feeds on the front page (layouts/home.html).

   Nothing from Instagram or TikTok is loaded until a visitor presses the
   button for that feed: an embed sets the platform's own cookies, and the
   privacy notice promises that the site sets none on its own. Pressing the
   button is the visitor's choice to load that one feed, and the note beside
   the buttons says what it does.

   Each button carries data-feed (instagram | tiktok) and data-user (the
   handle without @). The feed is placed in the box the button's
   aria-controls names, and the button is then taken away. */
(function () {
  "use strict";

  var tiktokScript = false;

  function instagram(box, user) {
    var frame = document.createElement("iframe");
    frame.src = "https://www.instagram.com/" + encodeURIComponent(user) + "/embed/";
    frame.title = "Instagram @" + user;
    frame.loading = "lazy";
    frame.className = "social-embed-frame";
    frame.setAttribute("allowtransparency", "true");
    box.appendChild(frame);
  }

  function tiktok(box, user) {
    var quote = document.createElement("blockquote");
    quote.className = "tiktok-embed";
    quote.setAttribute("cite", "https://www.tiktok.com/@" + user);
    quote.setAttribute("data-unique-id", user);
    quote.setAttribute("data-embed-type", "creator");
    var section = document.createElement("section");
    var link = document.createElement("a");
    link.href = "https://www.tiktok.com/@" + encodeURIComponent(user);
    link.target = "_blank";
    link.rel = "noopener";
    link.textContent = "@" + user;
    section.appendChild(link);
    quote.appendChild(section);
    box.appendChild(quote);
    // TikTok's script finds every .tiktok-embed on the page when it runs, so
    // it is added once, after the blockquote is in place.
    if (!tiktokScript) {
      tiktokScript = true;
      var script = document.createElement("script");
      script.src = "https://www.tiktok.com/embed.js";
      script.async = true;
      document.body.appendChild(script);
    }
  }

  var loaders = { instagram: instagram, tiktok: tiktok };

  document.addEventListener("click", function (event) {
    var button = event.target.closest("[data-feed]");
    if (!button) return;
    var load = loaders[button.getAttribute("data-feed")];
    var box = document.getElementById(button.getAttribute("aria-controls"));
    var user = button.getAttribute("data-user");
    if (!load || !box || !user) return;
    box.hidden = false;
    load(box, user);
    button.remove();
  });
})();
