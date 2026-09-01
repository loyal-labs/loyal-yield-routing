declare module "bn.js" {
  export default class BN {
    constructor(value?: string | number | bigint, base?: number);
    toNumber(): number;
    toString(base?: number): string;
    isZero(): boolean;
  }
}
