"use client";

import type { BridgeRecipient } from "@loyal-labs/api";
import { useRef, useState } from "react";

import { ThemedIcon } from "./themed-icon";

const BASE = "/wallet-workspace/facelift";

export function BridgeWalletIcon() {
  return (
    <span className="flex size-11 shrink-0 items-center justify-center rounded-[11px] bg-accent">
      <ThemedIcon
        className="size-7 text-tertiary"
        src={`${BASE}/bridge/wallet.svg`}
      />
    </span>
  );
}

export function shortBridgeAddress(address: string) {
  return `${address.slice(0, 4)}...${address.slice(-4)}`;
}

export function BridgeRecipientPane({
  network,
  isTron,
  recents,
  onSelect,
}: {
  network: string;
  isTron: boolean;
  recents: BridgeRecipient[] | null;
  onSelect: (address: string) => void;
}) {
  const [input, setInput] = useState("");
  const [submitted, setSubmitted] = useState(false);
  const [pasteError, setPasteError] = useState(false);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const address = input.trim();
  const valid = isTron
    ? /^T[1-9A-HJ-NP-Za-km-z]{33}$/.test(address)
    : /^0x[\da-fA-F]{40}$/.test(address);
  const invalid =
    address.length > 0 &&
    !valid &&
    (submitted ||
      /[^a-zA-Z0-9]/.test(address) ||
      address.length >= (isTron ? 34 : 42));

  const updateInput = (value: string, pasted = false) => {
    setInput(value.replace(/[\r\n]/g, ""));
    setSubmitted(pasted);
    setPasteError(false);
  };
  const paste = async () => {
    try {
      updateInput(await navigator.clipboard.readText(), true);
      inputRef.current?.focus({ preventScroll: true });
    } catch {
      setPasteError(true);
      inputRef.current?.focus({ preventScroll: true });
    }
  };
  const row = (value: string, subtitle: string) => (
    <button
      className="t-hover flex min-h-[60px] w-full items-center gap-3 rounded-2xl px-4 text-left hover:bg-accent focus-visible:bg-accent"
      key={value}
      onClick={() => onSelect(value)}
      type="button"
    >
      <BridgeWalletIcon />
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="truncate text-[16px] font-medium leading-5">
          {shortBridgeAddress(value)}
        </span>
        <span className="text-[13px] leading-4 text-muted-foreground">
          {network} · {subtitle}
        </span>
      </span>
    </button>
  );

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="px-6 pt-[30px] pb-2">
        <div className="flex min-h-12 items-center gap-3">
          <textarea
            aria-label={`${network} address`}
            aria-invalid={invalid}
            aria-describedby={invalid ? "bridge-address-error" : undefined}
            autoCapitalize="none"
            autoComplete="off"
            className="min-w-0 flex-1 resize-none bg-transparent text-[20px] font-medium leading-6 text-foreground outline-none placeholder:text-tertiary"
            maxLength={128}
            onChange={(event) => updateInput(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter") {
                event.preventDefault();
                setSubmitted(true);
                if (valid) onSelect(address);
              }
            }}
            onPaste={(event) => {
              event.preventDefault();
              updateInput(event.clipboardData.getData("text"), true);
            }}
            placeholder={`${network} address`}
            ref={inputRef}
            rows={address.length > 26 ? 2 : 1}
            spellCheck={false}
            value={input}
          />
          {input ? (
            <button
              aria-label="Clear address"
              className="t-hover flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent"
              onClick={() => {
                updateInput("");
                inputRef.current?.focus({ preventScroll: true });
              }}
              type="button"
            >
              <ThemedIcon
                className="size-6 text-tertiary"
                src={`${BASE}/bridge/clear.svg`}
              />
            </button>
          ) : (
            <button
              className="t-hover rounded-full bg-accent px-4 py-2.5 text-[13px] font-medium leading-4 hover:bg-accent-active"
              onClick={() => void paste()}
              type="button"
            >
              Paste
            </button>
          )}
        </div>
        {invalid && (
          <p
            className="pt-1 text-[16px] leading-5 text-destructive"
            id="bridge-address-error"
            role="alert"
          >
            Invalid {network} address
          </p>
        )}
        {pasteError && (
          <p className="pt-2 text-[13px] text-muted-foreground" role="status">
            Paste into the address field using your keyboard.
          </p>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        {valid ? (
          <>
            <p className="px-4 pt-3 pb-2 text-[16px] leading-5 text-muted-foreground">
              Exact match
            </p>
            {row(address, "New address")}
          </>
        ) : !address || invalid ? (
          <>
            {recents?.length !== 0 && (
              <p className="px-4 pt-3 pb-2 text-[16px] leading-5 text-muted-foreground">
                Recently used
              </p>
            )}
            {recents?.map(({ address: value, sends }) =>
              row(value, `${sends} ${sends === 1 ? "send" : "sends"}`)
            )}
            {recents?.length === 0 && (
              <div
                className="flex items-center gap-3 px-4 py-6 text-[16px] leading-5 text-muted-foreground"
                role="status"
              >
                <span
                  aria-hidden="true"
                  className="size-11 rounded-full border-2 border-dashed border-border"
                />
                No results found
              </div>
            )}
          </>
        ) : (
          <div
            className="flex items-center gap-3 px-4 py-6 text-[16px] leading-5 text-muted-foreground"
            role="status"
          >
            <span
              aria-hidden="true"
              className="size-11 rounded-full border-2 border-dashed border-border"
            />
            No results found
          </div>
        )}
      </div>
    </div>
  );
}
