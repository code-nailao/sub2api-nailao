// A deadline retires the round before reporting a timeout. The underlying
// request may still finish, but its callbacks no longer own the page.
export function requestRounds() {
  let current = 0;
  // An expired result is discarded, but a timeout is not a new user action.
  let latestStart = 0;
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
    waiting = latestStart = advance();
    return waiting;
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

  return { get current() { return current; }, get latestStart() { return latestStart; }, advance, start, isWaiting, finish, deadline };
}
