export class ProviderInterruptedError extends Error {
  constructor() {
    super("Provider turn was interrupted.");
    this.name = "ProviderInterruptedError";
  }
}
