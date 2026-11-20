export class Element extends EventTarget {
  constructor(tag) { super(); this.tag = tag; }
  children = [];
  dataset = {};
  attributes = {};
  open = false;
  disabled = false;
  append(...children) {
    for (const child of children) {
      if (child instanceof Element) child.parent = this;
      this.children.push(child);
    }
  }
  replaceChildren(...children) { this.children = []; this.append(...children); }
  querySelectorAll(selector) {
    const tags = selector.split(',');
    return this.children.filter((child) => child instanceof Element).flatMap((child) => [
      ...(tags.includes(child.tag) ? [child] : []), ...child.querySelectorAll(selector),
    ]);
  }
  setAttribute(key, value) { this.attributes[key] = value; }
  remove() { if (this.parent) this.parent.children = this.parent.children.filter((child) => child !== this); }
  showModal() { this.open = true; }
  close() {
    this.open = false;
    setImmediate(() => this.dispatchEvent(new Event('close')));
  }
  click() { if (!this.disabled) this.dispatchEvent(new Event('click')); }
}
