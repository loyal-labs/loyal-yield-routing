import { MMKV } from "react-native-mmkv";

const instance = new MMKV({ id: "loyal-app-storage" });

export const mmkv = {
  getString: (key: string): string | undefined => instance.getString(key),
  setString: (key: string, value: string): void => {
    instance.set(key, value);
  },
  getNumber: (key: string): number | undefined => instance.getNumber(key),
  setNumber: (key: string, value: number): void => {
    instance.set(key, value);
  },
  getBoolean: (key: string): boolean | undefined => instance.getBoolean(key),
  setBoolean: (key: string, value: boolean): void => {
    instance.set(key, value);
  },
  delete: (key: string): void => {
    instance.delete(key);
  },
  contains: (key: string): boolean => instance.contains(key),
};
