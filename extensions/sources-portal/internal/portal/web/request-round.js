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

export function connectionRounds() {
  const rounds = requestRounds();
  let docEpoch = 0;
  const capture = () => ({ round: rounds.current, generation: rounds.generation, docEpoch });
  const sameDocument = (started) => started.docEpoch === docEpoch;
  const sameConnection = (started) => sameDocument(started) && started.generation === rounds.generation;
  const isCurrent = (started) => sameConnection(started) && started.round === rounds.current;
  const captureConnection = () => {
    const started = capture();
    return () => sameConnection(started);
  };
  // Call only where the parent navigates or replaces the iframe.
  const newDocument = () => { docEpoch++; };
  return Object.assign(rounds, { capture, sameDocument, sameConnection, isCurrent, captureConnection, newDocument });
}
