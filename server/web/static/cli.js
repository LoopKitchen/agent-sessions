// This page obtains one Firebase ID token and hands it to the agent waiting on a
// loopback port of this machine. It is the sign-in page's sibling and differs
// from it in exactly one thing that matters: where the token goes. The dashboard
// posts to this origin and gets a session cookie; this posts to the listener
// internal/enroll bound before it opened this tab.
//
// Nothing here decides anything. The destination is not computed, not read from
// location.search and not recoverable from the query: the server rendered it
// from a port number it parsed and range-checked, and this module posts to that
// string or to nowhere. A page that took its own destination from its own URL
// would be an exfiltration primitive for a live credential, and a link is all it
// would take to aim it.
//
// The SDK version is pinned and must stay identical to signin.js. There is a Go
// test asserting the two agree, because two modules loading two SDK versions is
// the kind of drift that leaves the less-used page on an old release for a year
// with nothing looking wrong.
import { initializeApp } from "https://www.gstatic.com/firebasejs/12.8.0/firebase-app.js";
import {
  GoogleAuthProvider,
  getAuth,
  inMemoryPersistence,
  setPersistence,
  signInWithPopup,
} from "https://www.gstatic.com/firebasejs/12.8.0/firebase-auth.js";

const button = document.getElementById("cli-button");
const status = document.getElementById("cli-status");

const config = document.body.dataset;

// Read once, at load, so nothing later in this file can be handed a different
// destination by anything that mutates the DOM after the fact.
const callbackURL = config.callbackUrl;
const callbackState = config.callbackState;

const auth = getAuth(
  initializeApp({
    apiKey: config.apiKey,
    authDomain: config.authDomain,
    projectId: config.projectId,
  }),
);

const provider = new GoogleAuthProvider();
// People here routinely have a personal account signed in beside the work one.
// Enrolling the wrong identity produces a device whose sessions are attributed
// to somebody who is not on the roster, which reads as "the tool is broken"
// days later rather than as "I picked the wrong account" now.
provider.setCustomParameters({ prompt: "select_account" });

init().catch((err) => say(explain(err)));

async function init() {
  // The browser is left holding no Firebase credential of its own. This page
  // trades a single ID token for a device credential the agent stores, and a
  // refresh token persisted in IndexedDB would be a second, longer-lived
  // credential on the laptop that nothing here revokes or audits.
  await setPersistence(auth, inMemoryPersistence);
  button.addEventListener("click", onClick);
  button.disabled = false;
  say("");
}

async function onClick() {
  button.disabled = true;
  say("Waiting for Google…");
  try {
    const credential = await signInWithPopup(auth, provider);
    // Held in a local binding and never written to the DOM. The sign-in page
    // puts its token in a hidden input because a form post is how it travels;
    // this one has no form, so the token exists only for the length of this
    // call and an extension reading the document finds nothing.
    const idToken = await credential.user.getIdToken();
    say("Connecting to the agent on this computer…");
    await deliver(idToken);
    done();
  } catch (err) {
    button.disabled = false;
    say(explain(err));
  }
}

// deliver posts the token to the loopback listener.
//
// The JSON content type is deliberate on both sides: it is not a
// simple-request content type, so the browser must preflight, and the listener
// answers the preflight only for this server's origin. A form-encoded post
// would skip the preflight entirely and let any page on the internet reach that
// port.
async function deliver(idToken) {
  let res;
  try {
    res = await fetch(callbackURL, {
      method: "POST",
      mode: "cors",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id_token: idToken, state: callbackState }),
    });
  } catch (err) {
    // A fetch that rejects rather than answering means the browser declined to
    // make the request at all, which here is almost always the browser's local
    // network protection rather than a listener that has gone away.
    throw new Error("agent-unreachable");
  }
  if (res.ok) {
    return;
  }
  throw new Error("agent-" + res.status);
}

function done() {
  button.remove();
  say("You are signed in. You can close this tab and return to the terminal.");
}

function say(text) {
  status.textContent = text;
}

// explain renders a failure the person can act on, from an error code only.
// Firebase's error messages quote the request they came from, and the request
// this one came from carries an ID token.
function explain(err) {
  const code = (err && err.code) || (err && err.message) || "";
  switch (code) {
    case "auth/popup-closed-by-user":
    case "auth/cancelled-popup-request":
      return "Sign-in was cancelled. Nothing was connected.";
    case "auth/popup-blocked":
      return "Your browser blocked the sign-in window. Allow pop-ups for this site, then try again.";
    case "auth/network-request-failed":
      return "Could not reach Google. Check your connection and try again.";
    case "agent-unreachable":
      // Chrome treats an https page reaching 127.0.0.1 as a local network
      // request and may prompt for permission or refuse outright. Saying so is
      // the difference between a person clicking Allow and a person staring at
      // a page that appears to have hung.
      return "Could not reach the installer on this computer. If your browser asked " +
        "for permission to connect to a local device, allow it and try again. " +
        "Otherwise the installer may have stopped — run it again.";
    case "agent-403":
      return "The installer refused this sign-in. It was probably waiting for a " +
        "different one, or it timed out. Run the installer again.";
    default:
      if (code.startsWith("agent-")) {
        return "The installer could not read this sign-in. Run the installer again.";
      }
      return code ? `Sign-in failed (${code}). Try again.` : "Sign-in failed. Try again.";
  }
}
