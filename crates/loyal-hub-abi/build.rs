use std::{
    borrow::Borrow,
    env,
    fmt::Debug,
    fs,
    path::{Path, PathBuf},
};

const SCHEMA_PATH: &str = "schema/loyal_hub_abi.schema";

#[derive(Clone, Debug)]
struct Field {
    name: String,
    ty: String,
    offset: usize,
    size: usize,
}

#[derive(Clone, Debug)]
struct Repeat {
    name: String,
    ty: String,
    max_count: String,
    count_field: String,
    offset: usize,
    item_size: usize,
}

#[derive(Clone, Debug)]
struct Record {
    name: String,
    fields: Vec<Field>,
    repeats: Vec<Repeat>,
    fixed_len: usize,
    max_len: usize,
}

#[derive(Default)]
struct Schema {
    seeds: OrderedMap<String, String>,
    magic: OrderedMap<String, String>,
    limits: OrderedMap<String, usize>,
    instructions: OrderedMap<String, u8>,
    accounts: OrderedMap<String, OrderedMap<String, u8>>,
    records: OrderedMap<String, Record>,
    instruction_records: OrderedMap<String, String>,
}

/// Preserves order and rejects duplicate keys
struct OrderedMap<K, V> {
    entries: Vec<(K, V)>,
}

impl<K, V> Default for OrderedMap<K, V> {
    fn default() -> Self {
        Self {
            entries: Vec::new(),
        }
    }
}

impl<K, V> OrderedMap<K, V> {
    fn iter(&self) -> impl Iterator<Item = (&K, &V)> {
        self.entries.iter().map(|(key, value)| (key, value))
    }

    fn values(&self) -> impl Iterator<Item = &V> {
        self.entries.iter().map(|(_, value)| value)
    }
}

impl<K: Eq + Debug, V> OrderedMap<K, V> {
    fn insert(&mut self, key: K, value: V) {
        assert!(
            !self.entries.iter().any(|(existing, _)| existing == &key),
            "duplicate schema key {key:?}"
        );
        self.entries.push((key, value));
    }

    fn get<Q>(&self, key: &Q) -> Option<&V>
    where
        K: Borrow<Q>,
        Q: Eq + ?Sized,
    {
        self.entries
            .iter()
            .find(|(existing, _)| existing.borrow() == key)
            .map(|(_, value)| value)
    }

    fn get_or_insert_with(&mut self, key: K, value: impl FnOnce() -> V) -> &mut V {
        if let Some(index) = self
            .entries
            .iter()
            .position(|(existing, _)| existing == &key)
        {
            return &mut self.entries[index].1;
        }

        self.entries.push((key, value()));
        &mut self.entries.last_mut().expect("entry inserted").1
    }
}

fn main() {
    println!("cargo:rerun-if-changed={SCHEMA_PATH}");
    let schema = parse_schema(Path::new(SCHEMA_PATH));
    let generated = generate(&schema);
    let out_path = PathBuf::from(env::var_os("OUT_DIR").expect("OUT_DIR")).join("generated.rs");
    fs::write(out_path, generated).expect("write generated ABI");
}

fn parse_schema(path: &Path) -> Schema {
    let source = fs::read_to_string(path).expect("read Loyal Hub ABI schema");
    let mut schema = Schema::default();
    let mut current: Option<RecordBuilder> = None;

    for (line_number, raw_line) in source.lines().enumerate() {
        let line = raw_line.split('#').next().unwrap_or("").trim();
        if line.is_empty() {
            continue;
        }

        let parts = line.split_whitespace().collect::<Vec<_>>();
        match parts.as_slice() {
            ["seed", name, value] => {
                schema.seeds.insert((*name).to_owned(), (*value).to_owned());
            }
            ["magic", name, value] => {
                assert_eq!(
                    value.len(),
                    8,
                    "magic {name} must be exactly eight bytes at line {}",
                    line_number + 1
                );
                schema.magic.insert((*name).to_owned(), (*value).to_owned());
            }
            ["limit", name, value] => {
                schema.limits.insert(
                    (*name).to_owned(),
                    value.parse().unwrap_or_else(|_| {
                        panic!("invalid limit value at line {}", line_number + 1)
                    }),
                );
            }
            ["instruction", name, value] => {
                schema.instructions.insert(
                    (*name).to_owned(),
                    value.parse().unwrap_or_else(|_| {
                        panic!("invalid instruction tag at line {}", line_number + 1)
                    }),
                );
            }
            ["account", instruction, name, value] => {
                let index = value.parse().unwrap_or_else(|_| {
                    panic!("invalid account index at line {}", line_number + 1)
                });
                schema
                    .accounts
                    .get_or_insert_with((*instruction).to_owned(), OrderedMap::default)
                    .insert((*name).to_owned(), index);
            }
            ["record", name] => {
                assert!(
                    current.is_none(),
                    "nested record starts at line {}",
                    line_number + 1
                );
                current = Some(RecordBuilder::new(name));
            }
            ["field", name, ty] => current
                .as_mut()
                .expect("field outside record")
                .push_field(name, ty, &schema),
            ["repeat", name, ty, max_count, count_field] => current
                .as_mut()
                .expect("repeat outside record")
                .push_repeat(name, ty, max_count, count_field, &schema),
            ["end"] => {
                let builder = current
                    .take()
                    .unwrap_or_else(|| panic!("end outside record at line {}", line_number + 1));
                let record = builder.finish();
                schema.records.insert(record.name.clone(), record);
            }
            ["instruction_record", instruction, record] => {
                schema
                    .instruction_records
                    .insert((*instruction).to_owned(), (*record).to_owned());
            }
            _ => panic!("unrecognized schema line {}: {line}", line_number + 1),
        }
    }

    assert!(current.is_none(), "unterminated record in ABI schema");
    schema
}

struct RecordBuilder {
    name: String,
    fields: Vec<Field>,
    repeats: Vec<Repeat>,
    offset: usize,
}

impl RecordBuilder {
    fn new(name: &str) -> Self {
        Self {
            name: name.to_owned(),
            fields: Vec::new(),
            repeats: Vec::new(),
            offset: 0,
        }
    }

    fn push_field(&mut self, name: &str, ty: &str, schema: &Schema) {
        let size = type_size(ty, schema);
        self.fields.push(Field {
            name: name.to_owned(),
            ty: ty.to_owned(),
            offset: self.offset,
            size,
        });
        self.offset += size;
    }

    fn push_repeat(
        &mut self,
        name: &str,
        ty: &str,
        max_count: &str,
        count_field: &str,
        schema: &Schema,
    ) {
        let item_size = type_size(ty, schema);
        let max = *schema
            .limits
            .get(max_count)
            .unwrap_or_else(|| panic!("unknown max-count limit {max_count}"));
        self.repeats.push(Repeat {
            name: name.to_owned(),
            ty: ty.to_owned(),
            max_count: max_count.to_owned(),
            count_field: count_field.to_owned(),
            offset: self.offset,
            item_size,
        });
        self.offset += item_size * max;
    }

    fn finish(self) -> Record {
        let fixed_len = self
            .repeats
            .first()
            .map(|repeat| repeat.offset)
            .unwrap_or(self.offset);
        Record {
            name: self.name,
            fields: self.fields,
            repeats: self.repeats,
            fixed_len,
            max_len: self.offset,
        }
    }
}

fn type_size(ty: &str, schema: &Schema) -> usize {
    match ty {
        "bool" | "u8" => 1,
        "u16" => 2,
        "u64" => 8,
        "bytes8" => 8,
        "pubkey" => 32,
        other => {
            schema
                .records
                .get(&other.to_ascii_uppercase())
                .unwrap_or_else(|| panic!("unknown ABI field type {other}"))
                .max_len
        }
    }
}

fn generate(schema: &Schema) -> String {
    let mut output = String::new();
    output.push_str("// @generated by loyal-hub-abi/build.rs from schema/loyal_hub_abi.schema\n");
    output.push_str("// Do not edit this file by hand.\n\n");

    for (name, value) in schema.seeds.iter() {
        output.push_str(&format!("pub const {name}: &[u8] = b\"{value}\";\n"));
    }
    output.push('\n');
    for (name, value) in schema.magic.iter() {
        output.push_str(&format!("pub const {name}: &[u8; 8] = b\"{value}\";\n"));
    }
    output.push('\n');
    for (name, value) in schema.limits.iter() {
        output.push_str(&format!("pub const {name}: usize = {value};\n"));
    }
    output.push('\n');
    for (name, value) in schema.instructions.iter() {
        output.push_str(&format!("pub const {name}: u8 = {value};\n"));
    }
    output.push('\n');

    for (instruction, accounts) in schema.accounts.iter() {
        output.push_str(&format!(
            "pub mod {}_accounts {{\n",
            module_name(instruction)
        ));
        output.push_str("    #![allow(dead_code)]\n");
        for (name, index) in accounts.iter() {
            output.push_str(&format!("    pub const {name}: u8 = {index};\n"));
        }
        output.push_str("}\n\n");
    }

    for record in schema.records.values() {
        output.push_str(&record_module(record));
        output.push_str(&format!(
            "pub const {}_FIXED_LEN: usize = {}::FIXED_LEN;\n\
             pub const {}_MAX_LEN: usize = {}::MAX_LEN;\n\n",
            record.name,
            module_name(&record.name),
            record.name,
            module_name(&record.name),
        ));
    }

    for (instruction, record_name) in schema.instruction_records.iter() {
        let module = module_name(record_name);
        let prefix = instruction;
        output.push_str(&format!(
            "pub const {prefix}_TAG_OFFSET: u64 = 0;\n\
             pub const {prefix}_ARGS_OFFSET: usize = 1;\n\
             pub const {prefix}_ARGS_LEN: usize = {module}::MAX_LEN;\n\
             pub const {prefix}_DATA_LEN: usize = 1 + {prefix}_ARGS_LEN;\n"
        ));

        if let Some(record) = schema.records.get(record_name) {
            for field in &record.fields {
                output.push_str(&format!(
                    "pub const {prefix}_{}_DATA_OFFSET: u64 = (1 + {module}::{}_OFFSET) as u64;\n",
                    field.name, field.name
                ));
            }
            for repeat in &record.repeats {
                output.push_str(&format!(
                    "pub const {prefix}_{}_DATA_OFFSET: u64 = (1 + {module}::{}_OFFSET) as u64;\n",
                    repeat.name, repeat.name
                ));
            }
        }
        output.push('\n');
    }

    output
}

fn record_module(record: &Record) -> String {
    let module = module_name(&record.name);
    let mut output = String::new();
    output.push_str(&format!("pub mod {module} {{\n"));
    output.push_str("    #![allow(dead_code)]\n");
    for field in &record.fields {
        output.push_str(&format!(
            "    pub const {}_OFFSET: usize = {};\n",
            field.name, field.offset
        ));
        output.push_str(&format!(
            "    pub const {}_LEN: usize = {};\n",
            field.name, field.size
        ));
        output.push_str(&format!(
            "    pub const {}_TYPE: &str = \"{}\";\n",
            field.name, field.ty
        ));
        output.push('\n');
    }

    for repeat in &record.repeats {
        output.push_str(&format!(
            "    pub const {}_OFFSET: usize = {};\n",
            repeat.name, repeat.offset
        ));
        output.push_str(&format!(
            "    pub const {}_ITEM_LEN: usize = {};\n",
            repeat.name, repeat.item_size
        ));
        output.push_str(&format!(
            "    pub const {}_TYPE: &str = \"{}\";\n",
            repeat.name, repeat.ty
        ));
        output.push_str(&format!(
            "    pub const {}_MAX_COUNT: &str = \"{}\";\n",
            repeat.name, repeat.max_count
        ));
        output.push_str(&format!(
            "    pub const {}_COUNT_FIELD: &str = \"{}\";\n",
            repeat.name, repeat.count_field
        ));
        output.push('\n');
    }
    output.push_str(&format!(
        "    pub const FIXED_LEN: usize = {};\n",
        record.fixed_len
    ));
    output.push_str(&format!(
        "    pub const MAX_LEN: usize = {};\n",
        record.max_len
    ));
    output.push_str("}\n\n");
    output
}

fn module_name(name: &str) -> String {
    name.to_ascii_lowercase()
}
