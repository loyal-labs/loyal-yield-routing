import { readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const packageRoot = resolve(here, "..");
const repoRoot = resolve(packageRoot, "../..");
const schemaPath = resolve(repoRoot, "crates/loyal-hub-abi/schema/loyal_hub_abi.schema");
const outputPath = resolve(packageRoot, "src/generated/loyal-hub-abi.ts");

type Field = { name: string; kind: string };
type Repeat = { name: string; kind: string; maxCount: string; countField: string };
type RecordEntry =
  | { type: "field"; field: Field }
  | { type: "repeat"; repeat: Repeat };
type LayoutField = Field & { offset: number; len: number };
type LayoutRepeat = Repeat & { offset: number; itemLen: number };
type RecordLayout = {
  fields: LayoutField[];
  repeats: LayoutRepeat[];
  fixedLen: number;
  maxLen: number;
};

const source = readFileSync(schemaPath, "utf8");
const seeds = new Map<string, string>();
const magic = new Map<string, string>();
const limits = new Map<string, number>();
const instructions = new Map<string, number>();
const accounts = new Map<string, Map<string, number>>();
const records = new Map<string, RecordEntry[]>();
const instructionRecords = new Map<string, string>();
let currentRecord: string | undefined;

for (const [lineIndex, rawLine] of source.split("\n").entries()) {
  const lineNumber = lineIndex + 1;
  const line = rawLine.split("#")[0]?.trim();
  if (!line) {
    continue;
  }
  const parts = line.split(/\s+/);
  const [kind] = parts;
  switch (kind) {
    case "seed": {
      expectPartCount(parts, 3, lineNumber);
      insertUnique(seeds, parts[1], parts[2], "seed", lineNumber);
      break;
    }
    case "magic": {
      expectPartCount(parts, 3, lineNumber);
      if (parts[2].length !== 8) {
        throw new Error(`magic ${parts[1]} must be exactly eight bytes at line ${lineNumber}`);
      }
      insertUnique(magic, parts[1], parts[2], "magic", lineNumber);
      break;
    }
    case "limit": {
      expectPartCount(parts, 3, lineNumber);
      insertUnique(limits, parts[1], parseNonNegativeInteger(parts[2], "limit value", lineNumber), "limit", lineNumber);
      break;
    }
    case "instruction": {
      expectPartCount(parts, 3, lineNumber);
      insertUnique(instructions, parts[1], parseU8(parts[2], "instruction tag", lineNumber), "instruction", lineNumber);
      break;
    }
    case "account": {
      expectPartCount(parts, 4, lineNumber);
      const [, instruction, name, index] = parts;
      const byInstruction = accounts.get(instruction) ?? new Map<string, number>();
      insertUnique(byInstruction, name, parseU8(index, "account index", lineNumber), `account ${instruction}`, lineNumber);
      accounts.set(instruction, byInstruction);
      break;
    }
    case "record": {
      expectPartCount(parts, 2, lineNumber);
      if (currentRecord) {
        throw new Error(`nested record starts at line ${lineNumber}`);
      }
      currentRecord = parts[1];
      insertUnique(records, currentRecord, [], "record", lineNumber);
      break;
    }
    case "field": {
      expectPartCount(parts, 3, lineNumber);
      if (!currentRecord) {
        throw new Error(`field outside record at line ${lineNumber}`);
      }
      records.get(currentRecord)?.push({
        type: "field",
        field: { name: parts[1], kind: parts[2] },
      });
      break;
    }
    case "repeat": {
      expectPartCount(parts, 5, lineNumber);
      if (!currentRecord) {
        throw new Error(`repeat outside record at line ${lineNumber}`);
      }
      records.get(currentRecord)?.push({
        type: "repeat",
        repeat: {
          name: parts[1],
          kind: parts[2],
          maxCount: parts[3],
          countField: parts[4],
        },
      });
      break;
    }
    case "end": {
      expectPartCount(parts, 1, lineNumber);
      if (!currentRecord) {
        throw new Error(`end outside record at line ${lineNumber}`);
      }
      currentRecord = undefined;
      break;
    }
    case "instruction_record": {
      expectPartCount(parts, 3, lineNumber);
      insertUnique(instructionRecords, parts[1], parts[2], "instruction_record", lineNumber);
      break;
    }
    default:
      throw new Error(`unrecognized schema line ${lineNumber}: ${line}`);
  }
}
if (currentRecord) {
  throw new Error("unterminated record in ABI schema");
}

const recordLayouts = new Map<string, RecordLayout>();
for (const name of records.keys()) {
  recordLayout(name);
}

const generated = `// Generated from crates/loyal-hub-abi/schema/loyal_hub_abi.schema.
// Run \`bun run generate:abi\` in packages/loyal-actions after schema changes.

${[...seeds.entries()].map(([name, value]) => `export const ${name} = new Uint8Array([${bytesLiteral(value)}]);`).join("\n")}

${[...magic.entries()].map(([name, value]) => `export const ${name} = new Uint8Array([${bytesLiteral(value)}]);`).join("\n")}

${[...limits.entries()].map(([name, value]) => `export const ${name} = ${value};`).join("\n")}

${[...instructions.entries()].map(([name, value]) => `export const ${name} = ${value};`).join("\n")}

${[...accounts.entries()].map(([name, byInstruction]) => accountObject(name, byInstruction)).join("\n\n")}

${[...recordLayouts.entries()].map(([name, layout]) => recordConstants(name, layout)).join("\n\n")}

${[...instructionRecords.entries()].map(([name, recordName]) => instructionDataConstants(name, recordName)).join("\n\n")}
`;

writeFileSync(outputPath, generated);

function bytesLiteral(value: string): string {
  return [...new TextEncoder().encode(value)].join(", ");
}

function expectPartCount(parts: string[], expected: number, lineNumber: number): void {
  if (parts.length !== expected) {
    throw new Error(`expected ${expected} schema tokens at line ${lineNumber}, got ${parts.length}`);
  }
}

function insertUnique<K, V>(map: Map<K, V>, key: K, value: V, kind: string, lineNumber: number): void {
  if (map.has(key)) {
    throw new Error(`duplicate ${kind} key ${String(key)} at line ${lineNumber}`);
  }
  map.set(key, value);
}

function parseNonNegativeInteger(value: string, kind: string, lineNumber: number): number {
  if (!/^\d+$/.test(value)) {
    throw new Error(`invalid ${kind} at line ${lineNumber}`);
  }
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed)) {
    throw new Error(`${kind} is not a safe integer at line ${lineNumber}`);
  }
  return parsed;
}

function parseU8(value: string, kind: string, lineNumber: number): number {
  const parsed = parseNonNegativeInteger(value, kind, lineNumber);
  if (parsed > 255) {
    throw new Error(`${kind} must fit in u8 at line ${lineNumber}`);
  }
  return parsed;
}

function recordLayout(name: string): RecordLayout {
  const existing = recordLayouts.get(name);
  if (existing) {
    return existing;
  }
  const entries = records.get(name);
  if (!entries) {
    throw new Error(`missing record ${name}`);
  }
  const fields: LayoutField[] = [];
  const repeats: LayoutRepeat[] = [];
  let cursor = 0;
  for (const entry of entries) {
    if (entry.type === "field") {
      const len = typeLen(entry.field.kind);
      fields.push({ ...entry.field, offset: cursor, len });
      cursor += len;
      continue;
    }
    const itemLen = typeLen(entry.repeat.kind);
    const maxCount = limits.get(entry.repeat.maxCount);
    if (maxCount === undefined) {
      throw new Error(`missing repeat limit ${entry.repeat.maxCount}`);
    }
    repeats.push({ ...entry.repeat, offset: cursor, itemLen });
    cursor += itemLen * maxCount;
  }
  const layout = {
    fields,
    repeats,
    fixedLen: repeats[0]?.offset ?? cursor,
    maxLen: cursor,
  };
  recordLayouts.set(name, layout);
  return layout;
}

function typeLen(kind: string): number {
  switch (kind) {
    case "bool":
    case "u8":
      return 1;
    case "u16":
      return 2;
    case "u64":
      return 8;
    case "pubkey":
      return 32;
    case "bytes8":
      return 8;
    default:
      return recordLayout(kind.toUpperCase()).maxLen;
  }
}

function accountObject(instructionName: string, byInstruction: Map<string, number>): string {
  return `export const ${lowerCamel(instructionName)}Accounts = {
${[...byInstruction.entries()].map(([name, index]) => `  ${name}: ${index},`).join("\n")}
} as const;`;
}

function recordConstants(name: string, layout: RecordLayout): string {
  return [
    ...layout.fields.flatMap((field) => [
      `export const ${name}_${field.name}_OFFSET = ${field.offset};`,
      `export const ${name}_${field.name}_LEN = ${field.len};`,
    ]),
    ...layout.repeats.flatMap((repeat) => [
      `export const ${name}_${repeat.name}_OFFSET = ${repeat.offset};`,
      `export const ${name}_${repeat.name}_ITEM_LEN = ${repeat.itemLen};`,
    ]),
    `export const ${name}_FIXED_LEN = ${layout.fixedLen};`,
    `export const ${name}_MAX_LEN = ${layout.maxLen};`,
  ].join("\n");
}

function instructionDataConstants(instructionName: string, recordName: string): string {
  const layout = recordLayout(recordName);
  return [
    `export const ${instructionName}_TAG_OFFSET = 0;`,
    `export const ${instructionName}_ARGS_OFFSET = 1;`,
    `export const ${instructionName}_ARGS_LEN = ${layout.maxLen};`,
    `export const ${instructionName}_DATA_LEN = 1 + ${instructionName}_ARGS_LEN;`,
    ...layout.fields.map(
      (field) => `export const ${instructionName}_${field.name}_DATA_OFFSET = ${1 + field.offset};`,
    ),
    ...layout.repeats.map(
      (repeat) => `export const ${instructionName}_${repeat.name}_DATA_OFFSET = ${1 + repeat.offset};`,
    ),
  ].join("\n");
}

function lowerCamel(value: string): string {
  return value
    .toLowerCase()
    .replace(/_([a-z0-9])/g, (_, char: string) => char.toUpperCase());
}
