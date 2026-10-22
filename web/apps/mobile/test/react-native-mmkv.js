const stores = new Map();

class MMKV {
  constructor(config) {
    this.id = config?.id ?? "mmkv-default";
    if (!stores.has(this.id)) {
      stores.set(this.id, new Map());
    }
    this.store = stores.get(this.id);
  }

  getString(key) {
    const value = this.store.get(key);
    return typeof value === "string" ? value : undefined;
  }

  getNumber(key) {
    const value = this.store.get(key);
    return typeof value === "number" ? value : undefined;
  }

  getBoolean(key) {
    const value = this.store.get(key);
    return typeof value === "boolean" ? value : undefined;
  }

  set(key, value) {
    this.store.set(key, value);
  }

  delete(key) {
    this.store.delete(key);
  }

  contains(key) {
    return this.store.has(key);
  }
}

module.exports = { MMKV };
