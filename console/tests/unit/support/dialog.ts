if (!HTMLDialogElement.prototype.showModal) {
  HTMLDialogElement.prototype.showModal = function showModal() {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function close() {
    if (!this.open) return;
    this.open = false;
    this.dispatchEvent(new Event('close'));
  };
}
if (!HTMLElement.prototype.showPopover) {
  HTMLElement.prototype.showPopover = function showPopover() {
    this.dataset.popoverOpen = '';
  };
  HTMLElement.prototype.hidePopover = function hidePopover() {
    delete this.dataset.popoverOpen;
  };
}
