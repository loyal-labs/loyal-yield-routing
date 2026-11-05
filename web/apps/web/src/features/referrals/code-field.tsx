"use client";

import {
  useEffect,
  useId,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";

import { earnToast } from "@/components/wallet-workspace/facelift/earn-toast";
import { useAuthSession } from "@/contexts/auth-session-context";

import { claimCode, codeExists, readClaimWindow } from "./claim";
import {
  closesCopy,
  failureCopy,
  referralCopy,
  type ClaimFailure,
} from "./copy";
import {
  CODE_LENGTH,
  clearPendingCode,
  normalizeCode,
  readPendingCode,
  writePendingCode,
} from "./pending-code";

function CodeInput({
  value,
  onChange,
  error,
  action,
  autoFocus,
}: {
  value: string;
  onChange: (value: string) => void;
  error: ClaimFailure | null;
  action?: ReactNode;
  autoFocus?: boolean;
}) {
  const id = useId();
  return (
    <div className="flex flex-col gap-2">
      <label className="sr-only" htmlFor={id}>
        {referralCopy.label}
      </label>
      <div className="flex items-center gap-2">
        <input
          aria-describedby={`${id}-note`}
          aria-invalid={error !== null}
          autoFocus={autoFocus}
          autoCapitalize="characters"
          autoComplete="off"
          className="h-12 min-w-0 flex-1 rounded-full bg-secondary px-4 font-mono text-sm uppercase tracking-wider outline-none placeholder:font-sans placeholder:normal-case placeholder:tracking-normal placeholder:text-tertiary"
          id={id}
          onChange={(event) => onChange(event.target.value)}
          placeholder={referralCopy.placeholder}
          spellCheck={false}
          type="text"
          value={value}
        />
        {action}
      </div>
      <p
        className={`px-1 text-[13px] leading-4 ${
          error ? "text-destructive" : "text-muted-foreground"
        }`}
        id={`${id}-note`}
        role={error ? "alert" : undefined}
      >
        {error ? failureCopy[error] : referralCopy.visibility}
      </p>
    </div>
  );
}

function SignInCodeField({ autoFocus }: { autoFocus: boolean }) {
  const [value, setValue] = useState(() => readPendingCode()?.code ?? "");
  const [error, setError] = useState<ClaimFailure | null>(null);
  const latest = useRef(value);

  function change(raw: string) {
    latest.current = raw;
    setValue(raw);
    setError(null);
    const code = normalizeCode(raw);
    if (!code) {
      clearPendingCode();
      if (raw.trim().length >= CODE_LENGTH) setError("malformed");
      return;
    }
    writePendingCode({ code, source: "typed" });
    void codeExists(code).then((exists) => {
      if (exists !== false || latest.current !== raw) return;
      clearPendingCode();
      setError("unknown_code");
    });
  }

  return (
    <CodeInput
      autoFocus={autoFocus}
      error={error}
      onChange={change}
      value={value}
    />
  );
}

function InviteCodeRow() {
  const [open, setOpen] = useState(() => readPendingCode() !== null);
  const [pressed, setPressed] = useState(false);
  if (open) return <SignInCodeField autoFocus={pressed} />;
  return (
    <button
      className="t-hover self-start rounded-full px-3 py-1.5 text-[14px] text-muted-foreground leading-5 hover:bg-accent hover:text-foreground"
      onClick={() => {
        setPressed(true);
        setOpen(true);
      }}
      type="button"
    >
      {referralCopy.haveCode}
    </button>
  );
}

export function SignInCodeRow() {
  const { launched } = useAuthSession();
  return launched ? <InviteCodeRow /> : null;
}

export function SettingsCodeField() {
  const [closesAt, setClosesAt] = useState<string | null>(null);
  const [value, setValue] = useState("");
  const [error, setError] = useState<ClaimFailure | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    void readClaimWindow().then((next) => {
      if (!cancelled) setClosesAt(next);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  if (closesAt === null) return null;

  async function submit(event: FormEvent) {
    event.preventDefault();
    const code = normalizeCode(value);
    if (!code) {
      setError("malformed");
      return;
    }
    setBusy(true);
    setError(null);
    const outcome = await claimCode({ code, source: "typed" });
    setBusy(false);
    if (outcome.kind === "bound") {
      earnToast.success(referralCopy.joined);
      setClosesAt(null);
    } else {
      setError(outcome.failure);
    }
  }

  return (
    <form className="flex flex-col gap-3 px-4 py-3" onSubmit={submit}>
      <div>
        <p className="font-medium text-[16px] leading-5 tracking-[-0.176px]">
          {referralCopy.label}
        </p>
        <p className="mt-0.5 text-[13px] text-muted-foreground leading-4">
          {closesCopy(closesAt)}
        </p>
      </div>
      <CodeInput
        action={
          <button
            className="t-hover h-12 shrink-0 rounded-full bg-accent px-5 font-medium text-[14px] enabled:hover:bg-accent-selected disabled:opacity-40"
            disabled={busy || value.length === 0}
            type="submit"
          >
            {busy ? referralCopy.applying : referralCopy.apply}
          </button>
        }
        error={error}
        onChange={(raw) => {
          setValue(raw);
          setError(null);
        }}
        value={value}
      />
    </form>
  );
}
