// A deadline retires the round before reporting a timeout. The underlying
// request may still finish, but its callbacks no longer own the page.
export function requestRounds() {
  let current = 0;
  // A new attempt or an accepted connection supersedes an earlier save.
  // A timeout retires the request but preserves the connection generation.
  let generation = 0;
  let waiting;
  let timer;

  function stopWaiting() {
    clearTimeout(timer);
    timer = waiting = undefined;
  }
  function advance() {
    stopWaiting();
    return ++current;
  }
  function start() {
    generation++;
    waiting = advance();
    return waiting;
  }
  function markApplied() {
    generation++;
  }
  function isWaiting(round = current) {
    return waiting === round;
  }
  function finish(round) {
    if (!isWaiting(round)) return false;
    stopWaiting();
    return true;
  }
  function deadline(round, delay, onTimeout, stillCurrent = () => true) {
    if (!isWaiting(round)) return;
    clearTimeout(timer);
    const scheduled = setTimeout(() => {
      // The guard is the caller's staleness test, applied before the timeout
      // retires the attempt: an expired timer must not change anything.
      if (timer !== scheduled || !isWaiting(round) || !stillCurrent()) return;
      advance();
      onTimeout();
    }, delay);
    timer = scheduled;
  }

  return { get current() { return current; }, get generation() { return generation; }, advance, start, markApplied, isWaiting, finish, deadline };
}

// Round rules for the Portal pages. isCurrent(token) is the only staleness
// test; the pages never compare a round, generation or document themselves.
// A token records what its request depended on, and its factory says what the
// result is allowed to change:
//
//   beginAttempt(scope)  the page's one connection attempt. A new attempt or an
//                        accepted connection ends it. Scope "page" when the page
//                        started it, "view" when the child asked for it: that
//                        one also ends with the document that asked.
//   beginRead(kind)      the surface the child is showing, plus a serial per
//                        kind so only the latest read of that kind applies.
//   beginChild(key)      an answer owed to one document about one connection
//                        (a task grant restore). It ends with that document,
//                        with a newer answer for the same connection, or with
//                        an authorization the page accepted for it. A retry,
//                        poll or acknowledgement elsewhere does not end it.
//   beginVerdict()       a verdict a child answer established (this connection
//                        needs a resave). It ends with its document only, so a
//                        failure for the same connection does not take away its
//                        guidance; a successful answer ends it where it is set.
//   captureConnection()  a configuration the child accepted: ends with the
//                        connection generation or the document showing it.
//   beginView()          a request the page itself makes on the child's
//                        behalf (a shell payload): ends with the document.
//   guardConnection()    a save that outlives its dialog: ends with the
//                        connection or the document, not with a timeout.
//
// generation moves only on beginAttempt and accepted(); document only on
// newDocument(); a key only on beginChild and renew for that key. A deadline
// timeout retires the attempt (retire()) without moving generation.
export function connectionRounds() {
  const rounds = requestRounds();
  let document = 0;
  const keys = new Map();
  const reads = new Map();

  function token(scope) {
    return { scope, round: rounds.current, generation: rounds.generation, document };
  }
  function beginAttempt(scope = "page") {
    rounds.start();
    return token(scope);
  }
  function beginView() {
    return token("view");
  }
  function beginRead(kind) {
    const serial = (reads.get(kind) || 0) + 1;
    reads.set(kind, serial);
    return { ...token("view"), kind, serial };
  }
  function beginChild(key) {
    const revision = (keys.get(key) || 0) + 1;
    keys.set(key, revision);
    return { scope: "child", document, key, revision };
  }
  function beginVerdict() {
    return { scope: "verdict", document };
  }
  function renew(key) {
    keys.set(key, (keys.get(key) || 0) + 1);
  }
  const captureConnection = () => token("connection");
  // A predicate for code that asks later (a save that outlives its dialog).
  const guardConnection = () => {
    const started = token("connection");
    return () => isCurrent(started);
  };

  function isCurrent(started) {
    if (!started) return false;
    if (started.scope === "child") {
      return started.document === document && keys.get(started.key) === started.revision;
    }
    if (started.scope === "verdict") return started.document === document;
    if (started.scope === "connection") {
      return started.generation === rounds.generation && started.document === document;
    }
    if (started.round !== rounds.current || started.generation !== rounds.generation) return false;
    if (started.scope === "page") return true;
    if (started.document !== document) return false;
    return started.kind === undefined || reads.get(started.kind) === started.serial;
  }
  // A result still owed after the child was re-addressed: same scope, current
  // counters. A child key keeps its own revision.
  function readdress(started) {
    if (started.scope === "child") return started;
    return { ...started, round: rounds.current, generation: rounds.generation, document };
  }
  const isWaiting = (started) => Boolean(started) && rounds.isWaiting(started.round);
  const finish = (started) => rounds.finish(started.round);
  const deadline = (started, delay, onTimeout) => rounds.deadline(started.round, delay, onTimeout,
    () => isCurrent(started));
  const retire = () => rounds.advance();
  const accepted = () => rounds.markApplied();
  // The one place a child document begins. A bare load is not a document.
  const newDocument = () => { document++; };

  // A new object: the base methods must not be overwritten by their token
  // wrappers above.
  return {
    get current() { return rounds.current; }, get generation() { return rounds.generation; }, get document() { return document; },
    advance: rounds.advance, start: rounds.start, markApplied: rounds.markApplied,
    isWaiting, finish, deadline, beginAttempt, beginView, beginRead, beginChild, beginVerdict, renew,
    captureConnection, beginView, guardConnection, isCurrent, readdress, retire, accepted, newDocument,
  };
}
