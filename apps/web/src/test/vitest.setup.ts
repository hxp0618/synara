// Node 26 exposes an experimental, configurable localStorage accessor whose value is undefined
// unless --localstorage-file is provided. Persistent Zustand stores capture that undefined value
// during module initialization, before individual tests can install their own storage doubles.

function createMemoryStorage(): Storage {
  const values = new Map<string, string>();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
    removeItem: (key) => values.delete(key),
    clear: () => values.clear(),
    key: (index) => [...values.keys()][index] ?? null,
    get length() {
      return values.size;
    },
  } as Storage;
}

Object.defineProperty(globalThis, "localStorage", {
  configurable: true,
  enumerable: true,
  writable: true,
  value: createMemoryStorage(),
});
