// combo.js upgrades the dashboard's own dropdown so it prunes as you type.
//
// The panel itself is CSS: .combo:focus-within shows it, every option is a
// real <a href> that applies itself, and Enter submits the form. That is the
// whole no-JS behaviour and it stays intact. All this file does is hide the
// options that do not match what has been typed, and add arrow-key
// highlighting. Turn the script off and the component degrades to a full,
// clickable list.
//
// The alternative was a native <input list> + <datalist>, which prunes for
// free but draws a browser popup no stylesheet can reach. Owner's call on
// 2026-08-13: theme AND function. This is the smallest thing that buys both.
//
// No dependencies. Nothing here builds markup from a string, evaluates one, or
// fetches anything; it moves classes around on nodes the server already
// rendered. TestTheOneScriptStaysSmallAndInert scans this file for the names
// of those constructs, so do not spell them even in a comment.
(function () {
  "use strict";

  // Matching is substring, not prefix: a datalist matches "ann" against the
  // start of ann@example.com only, so nobody could find themselves by typing
  // their surname or the repo half of an owner/repo pair. Substring is a
  // superset of what the browser did, so this never prunes away a row the
  // native widget would have kept.
  function matches(option, needle) {
    return option.textContent.toLowerCase().indexOf(needle) !== -1;
  }

  // Unique enough for aria-activedescendant, which needs an id to point at.
  var seq = 0;

  function setup(root) {
    var input = root.querySelector("input");
    var panel = root.querySelector(".combo-panel");
    if (!input || !panel) {
      return;
    }
    var options = Array.prototype.slice.call(panel.querySelectorAll("a"));
    if (!options.length) {
      return;
    }
    // A combo marked data-fill feeds a form field instead of navigating: the
    // settings destination picker posts what is typed and the server resolves
    // it. Its options are still real links to the same page carrying the same
    // value, so a scriptless browser gets there in one round trip.
    var fills = root.hasAttribute("data-fill");
    // Pruning starts only once somebody types. On a filter page the box is
    // pre-filled with the filter already applied, and pruning to that on load
    // would hide every option including the one that clears it.
    var typed = false;
    var active = -1;

    // Semantics, added only on the scripted path. The datalist this replaced
    // carried them natively, and dropping them silently was the accessibility
    // cost of owning the widget. A screen reader with the script blocked still
    // meets a labelled text input and a list of links, which is honest about
    // what the no-JS component actually is.
    seq++;
    if (!panel.id) {
      panel.id = "combo-panel-" + seq;
    }
    input.setAttribute("role", "combobox");
    input.setAttribute("aria-autocomplete", "list");
    input.setAttribute("aria-controls", panel.id);
    input.setAttribute("aria-expanded", "false");
    panel.setAttribute("role", "listbox");
    options.forEach(function (o, i) {
      // Options leave the tab order once the arrow keys can reach them.
      // Otherwise Tab walks every channel in the workspace one at a time,
      // which the datalist never did and which makes the filter bar
      // unusable from the keyboard the moment a directory is long.
      o.setAttribute("tabindex", "-1");
      o.setAttribute("role", "option");
      if (!o.id) {
        o.id = panel.id + "-opt-" + i;
      }
    });

    // aria-expanded tracks focus rather than computed style: the panel also
    // opens on hover, and a pointer user hovering it is not a state a screen
    // reader is narrating.
    function expanded(open) {
      input.setAttribute("aria-expanded", open ? "true" : "false");
    }

    function visible() {
      return options.filter(function (o) {
        return !o.classList.contains("hide");
      });
    }

    function highlight(next) {
      if (active >= 0 && options[active]) {
        options[active].classList.remove("active");
        options[active].removeAttribute("aria-selected");
      }
      active = next;
      if (active >= 0 && options[active]) {
        options[active].classList.add("active");
        options[active].setAttribute("aria-selected", "true");
        input.setAttribute("aria-activedescendant", options[active].id);
        options[active].scrollIntoView({ block: "nearest" });
        return;
      }
      input.removeAttribute("aria-activedescendant");
    }

    function reset() {
      typed = false;
      highlight(-1);
      options.forEach(function (o) {
        o.classList.remove("hide");
      });
    }

    function prune() {
      typed = true;
      highlight(-1);
      var needle = input.value.trim().toLowerCase();
      options.forEach(function (o) {
        o.classList.toggle("hide", needle !== "" && !matches(o, needle));
      });
    }

    // An option may say more than it means: the people picker lists
    // "@handle — Real Name" so the name is searchable, while the field wants
    // the handle alone. data-value carries that, and the option's href carries
    // the identical string, so the scripted and scriptless paths fill the box
    // with the same text.
    //
    // Filling has to close the panel. A navigating combo closes by leaving the
    // page; this one stays put, and a panel left open hangs a 16rem list over
    // whatever is below it. On the settings page that is the Create group
    // button, so the click meant to submit the form landed on another option
    // and silently rewrote the pick instead.
    //
    // The picked class is what does it, and it does it alone: every rule that
    // opens the panel is out-ranked by a .picked rule at equal specificity and
    // later source order, focus and hover included. So the panel can shut with
    // focus still in the box, which is where focus belongs after a keyboard
    // pick. Blurring to close it instead threw focus to the document body and
    // made the next Tab restart from the top of the page, which is the same
    // keyboard trap the tabindex work above exists to prevent.
    function close() {
      reset();
      root.classList.add("picked");
    }

    function open() {
      root.classList.remove("picked");
      expanded(true);
    }

    function choose(option) {
      if (!fills) {
        return false;
      }
      input.value = option.getAttribute("data-value") || option.textContent.trim();
      close();
      return true;
    }

    function step(delta) {
      var shown = visible();
      if (!shown.length) {
        return;
      }
      var at = shown.indexOf(options[active]);
      at = at < 0 ? (delta > 0 ? 0 : shown.length - 1) : at + delta;
      if (at < 0) {
        at = shown.length - 1;
      } else if (at >= shown.length) {
        at = 0;
      }
      highlight(options.indexOf(shown[at]));
    }

    input.addEventListener("input", function () {
      prune();
      open();
    });
    // Reopening a box that was left pruned should show the whole list again,
    // for the same reason it is not pruned on load.
    input.addEventListener("focus", function () {
      if (!typed) {
        reset();
      }
      open();
    });
    input.addEventListener("blur", function () {
      reset();
      expanded(false);
    });
    // The pointer leaving releases a picked panel, but only while the box is
    // not focused. Releasing it with focus still inside would hand the panel
    // straight back to :focus-within and pop it open again, which is the whole
    // reason this used to blur. Focused pickers are released by the focus and
    // input handlers above instead.
    root.addEventListener("mouseleave", function () {
      if (document.activeElement !== input) {
        root.classList.remove("picked");
      }
    });
    // Clicking a box that already has focus fires no focus event, so this is
    // the only thing that reopens the panel after a pick without typing.
    input.addEventListener("mousedown", open);

    input.addEventListener("keydown", function (e) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        step(1);
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        step(-1);
      } else if (e.key === "Escape") {
        // The one place blurring is right: Escape means leave the widget, not
        // just shut the list.
        close();
        input.blur();
      } else if (e.key === "Enter") {
        // Enter with nothing highlighted submits the form, which is the
        // server-side filter and exactly what this page did before.
        var option = active >= 0 ? options[active] : null;
        if (!option || option.classList.contains("hide")) {
          return;
        }
        e.preventDefault();
        if (!choose(option)) {
          window.location.assign(option.href);
        }
      }
    });

    // Keeping focus in the input through the press is what makes clicking an
    // option land where it was aimed. Without this the blur fires between
    // mousedown and click, reset() un-hides every pruned option, the panel
    // grows under the pointer and the click resolves against whatever moved
    // into that spot. It also keeps :focus-within true, so the panel does not
    // blink shut mid-press.
    panel.addEventListener("mousedown", function (e) {
      e.preventDefault();
    });

    panel.addEventListener("click", function (e) {
      var option = e.target.closest ? e.target.closest("a") : null;
      if (!option || !panel.contains(option)) {
        return;
      }
      // Navigating combos let the click through: the href is the filter.
      if (choose(option)) {
        e.preventDefault();
      }
    });
  }

  var roots = document.querySelectorAll(".combo");
  for (var i = 0; i < roots.length; i++) {
    setup(roots[i]);
  }
})();
