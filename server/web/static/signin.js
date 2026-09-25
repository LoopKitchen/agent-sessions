// The sign-in page's whole job is to obtain one Firebase ID token and hand it to
// the server. Nothing here decides anything: the domain check, the roster check
// and the session cookie all happen in server/app/signin.go, against a token
// whose signature has been verified there. Treat every line below as hostile
// input to that endpoint, because a browser extension can rewrite it.
//
// The SDK comes from Google's own CDN, at a pinned version, because that is
// where the organisation's other frontends take it from and because the page's
// Content-Security-Policy names exactly that origin. Adding a third-party CDN
// would put the code that handles a live credential on somebody else's release
// process, and the policy would refuse to load it anyway.
import { initializeApp } from "https://www.gstatic.com/firebasejs/12.8.0/firebase-app.js";
import {
  GoogleAuthProvider,
  getAuth,
  inMemoryPersistence,
  setPersistence,
  signInWithPopup,
} from "https://www.gstatic.com/firebasejs/12.8.0/firebase-auth.js";

const form = document.getElementById("signin");
const button = document.getElementById("signin-button");
const status = document.getElementById("signin-status");

// The Firebase web config is public by design — it identifies the project, it
// does not authorise anything — so it is rendered into the page rather than
// fetched, which keeps a round trip out of the path of an interactive sign-in.
const config = document.body.dataset;
const auth = getAuth(
  initializeApp({
    apiKey: config.apiKey,
    authDomain: config.authDomain,
    projectId: config.projectId,
  }),
);

const provider = new GoogleAuthProvider();
// People here routinely have a personal account signed in beside the work one.
// Silently taking whichever Google picks produces a refusal from a server that
// cannot say which account was wrong, and that reads as "the tool is broken".
provider.setCustomParameters({ prompt: "select_account" });

init().catch((err) => say(explain(err)));

async function init() {
  // The browser is left holding no Firebase credential of its own. This page
  // trades a single ID token for the server's session cookie, and that cookie is
  // the session; a refresh token persisted in IndexedDB would be a second,
  // longer-lived credential on a laptop that nothing here revokes or audits.
  //
  // It also keeps the token's `iat` meaning what the server reads it as. With no
  // persisted session every token is minted at the moment somebody completes the
  // popup, so the absolute session cap the server measures from it is measured
  // from an actual authentication rather than from an hourly refresh.
  await setPersistence(auth, inMemoryPersistence);
  form.addEventListener("submit", onSubmit);
  button.disabled = false;
  say("");
}

async function onSubmit(event) {
  event.preventDefault();
  button.disabled = true;
  say("Waiting for Google…");
  try {
    const credential = await signInWithPopup(auth, provider);
    form.elements.id_token.value = await credential.user.getIdToken();
    // submit() rather than requestSubmit(): this runs inside the submit handler,
    // and requestSubmit would fire it again.
    form.submit();
  } catch (err) {
    button.disabled = false;
    say(explain(err));
  }
}

function say(text) {
  status.textContent = text;
}

// explain renders a failure the person can act on, from the error code only.
// Firebase's error messages quote the request they came from, and the request
// this one came from carries an ID token.
function explain(err) {
  const code = (err && err.code) || "";
  switch (code) {
    case "auth/popup-closed-by-user":
    case "auth/cancelled-popup-request":
      return "Sign-in was cancelled.";
    case "auth/popup-blocked":
      return "Your browser blocked the sign-in window. Allow pop-ups for this site, then try again.";
    case "auth/network-request-failed":
      return "Could not reach Google. Check your connection and try again.";
    default:
      return code ? `Sign-in failed (${code}). Try again.` : "Sign-in failed. Try again.";
  }
}
