use anyhow::{bail, Context, Result};
use chrono::{DateTime, Utc};
use prost::Message;
use serde::Deserialize;

const FILTERED_ACCOUNTS_TYPE_URL: &str =
    "type.googleapis.com/sf.substreams.solana.type.v1.FilteredAccounts";

#[derive(Debug, Deserialize)]
#[serde(tag = "kind")]
pub enum SubstreamsGrpcAdapterEvent {
    #[serde(rename = "session")]
    Session {
        trace_id: String,
        resolved_start_block: u64,
        linear_handoff_block: u64,
        max_parallel_workers: u64,
        chain_head: u64,
    },
    #[serde(rename = "progress")]
    Progress(SubstreamsProgressUpdate),
    #[serde(rename = "block")]
    Block(SubstreamsBlockOutput),
    #[serde(rename = "undo")]
    Undo { last_valid_block: u64 },
    #[serde(rename = "complete")]
    Complete { cursor: String },
    #[serde(rename = "error")]
    Error { message: String },
}

#[derive(Clone, Debug, Deserialize)]
pub struct SubstreamsProgressUpdate {
    pub highest_contiguous_block: Option<u64>,
    pub processed_blocks: u64,
    pub total_bytes_read: u64,
    pub total_bytes_written: u64,
    pub completed_range_count: usize,
    pub running_job_count: usize,
}

#[derive(Debug, Deserialize)]
pub struct SubstreamsBlockOutput {
    pub block: u64,
    pub timestamp: Option<DateTime<Utc>>,
    pub type_url: String,
    pub value: String,
}

#[derive(Clone, Debug)]
pub struct SubstreamsAccountUpdate {
    pub slot: u64,
    pub observed_at: Option<DateTime<Utc>>,
    pub accounts: Vec<SubstreamsAccount>,
}

#[derive(Clone, Debug)]
pub struct SubstreamsAccount {
    pub address: Vec<u8>,
    pub owner: Vec<u8>,
    pub data: Vec<u8>,
    pub deleted: bool,
}

pub fn decode_block_output(
    output: SubstreamsBlockOutput,
    decode_base64: impl FnOnce(&str) -> Result<Vec<u8>>,
) -> Result<SubstreamsAccountUpdate> {
    if output.type_url != FILTERED_ACCOUNTS_TYPE_URL {
        bail!("unexpected Substreams output type {}", output.type_url);
    }
    let value = decode_base64(&output.value).context("decode Substreams gRPC block output")?;
    let accounts = solana::FilteredAccounts::decode(value.as_slice())
        .context("decode Substreams filtered accounts output")?
        .accounts
        .into_iter()
        .map(|account| SubstreamsAccount {
            address: account.address,
            owner: account.owner,
            data: account.data,
            deleted: account.deleted,
        })
        .collect::<Vec<_>>();
    Ok(SubstreamsAccountUpdate {
        slot: output.block,
        observed_at: output.timestamp,
        accounts,
    })
}

pub mod solana {
    use super::*;

    #[derive(Clone, PartialEq, Message)]
    pub struct FilteredAccounts {
        #[prost(message, repeated, tag = "1")]
        pub accounts: Vec<Account>,
    }

    #[derive(Clone, PartialEq, Message)]
    pub struct Account {
        #[prost(bytes, tag = "1")]
        pub address: Vec<u8>,
        #[prost(bytes, tag = "2")]
        pub owner: Vec<u8>,
        #[prost(bytes, tag = "3")]
        pub data: Vec<u8>,
        #[prost(bool, tag = "7")]
        pub deleted: bool,
    }
}
