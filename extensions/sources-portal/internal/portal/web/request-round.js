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
  function deadline(round, delay, onTimeout) {
    if (!isWaiting(round)) return;
    clearTimeout(timer);
    const scheduled = setTimeout(() => {
      if (timer !== scheduled || !isWaiting(round)) return;
      advance();
      onTimeout();
    }, delay);
    timer = scheduled;
  }

  return { get current() { return current; }, get generation() { return generation; }, advance, start, markApplied, isWaiting, finish, deadline };
}

// Round rules for the Portal pages. Every asynchronous result carries the
// token capture() returned when its request started, and is applied through
// one comparison: each counter in the token against its current value. Which
// counters a result depends on is fixed by what the result may change:
//
//   page        round + generation. Work the page itself started: authorizing
//               or reading a group, saving a connection. Another attempt or an
//               accepted connection supersedes it. The embedded page starting
//               a new document does not: the parent's request is still owed.
//   connection  generation + document. The connection the child accepted: its
//               acknowledgement and a pending confirmation. It ends when the
//               connection changes or the document that ran it is replaced.
//   document    document only. An answer owed to one child document: a restore
//               of a task's grant, and any page state derived from it.
//
// generation advances in exactly two places: start() of a page connection
// attempt and markApplied() of an accepted acknowledgement. document advances
// only in newDocument(). Nothing else - acknowledgements, polls, save results,
// restores - advances either.
export function connectionRounds() {
  const rounds = requestRounds();
  let document = 0;
  const capture = () => ({ round: rounds.current, generation: rounds.generation, document });
  const samePage = (started) => started.round === rounds.current && started.generation === rounds.generation;
  const sameConnection = (started) => started.generation === rounds.generation && started.document === document;
  const sameDocument = (started) => started.document === document;
  const isCurrent = (started) => samePage(started) && sameDocument(started);
  // A save outlives the child document; it ends with the connection it changed.
  const captureConnection = () => {
    const started = capture();
    return () => started.generation === rounds.generation;
  };
  // Call where the parent navigates or replaces the iframe, where the child
  // announces a new document, and where a load shows one the parent did not
  // navigate. The load that belongs to the opened document is not a boundary.
  const newDocument = () => { document++; };
  return Object.assign(rounds, { get document() { return document; }, capture, samePage, sameDocument, sameConnection, isCurrent, captureConnection, newDocument });
}
