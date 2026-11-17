import type { CSSProperties, ReactNode } from "react";
import type { Quantity, UnitLabels } from "../ports.js";
import { formatQuantity, formatSigned, formatWithUnit, type MeterModel, type Tone } from "../model.js";

/** Where a person can go to fix something: a label and a URL. The host decides which; the package only draws it. */
export interface Remedy {
  readonly label: string;
  readonly href: string;
}

/**
 * A URL is a link only if it cannot run script: http(s), mailto, or a path or fragment of this site. Anything else — `javascript:`,
 * `data:`, a protocol-relative `//host` — is not drawn as a link, because these strings come from a host's data, and from there
 * from whoever can write it.
 */
export function safeHref(href: string): string | null {
  const t = href.trim();
  if (/^(https?:|mailto:)/i.test(t)) return t;
  if ((t.startsWith("/") && !t.startsWith("//")) || t.startsWith("#")) return t;
  return null;
}

/** A figure as text, with the exact integer it was drawn from beside it: what is shown may be rounded, what is carried never is. */
export function Figure({ q, labels, signed = false, unit = false, compact = false }: { q: Quantity; labels: UnitLabels; signed?: boolean; unit?: boolean; compact?: boolean }) {
  const text = signed ? formatSigned(q, labels) : unit ? formatWithUnit(q, labels, { compact }) : formatQuantity(q, labels);
  return (
    <span className="cu-figure" data-quantity={String(q)}>
      {text}
    </span>
  );
}

/** A link only when the destination is safe to follow; otherwise the label alone, so nothing is silently dropped. */
export function SafeLink({ to, children, ...rest }: { to: Remedy | { href: string }; children: ReactNode } & Record<`data-${string}`, string>) {
  const href = safeHref(to.href);
  return href === null ? <span {...rest}>{children}</span> : <a href={href} {...rest}>{children}</a>;
}

/** The one meter. The painted width and the ARIA value both come from the same whole percent. */
export function Meter({ meter, tone, label, valueText }: { meter: MeterModel; tone: Tone; label: string; valueText: string }) {
  return (
    <div className="cu-meter" role="progressbar" aria-label={label} aria-valuemin={0} aria-valuemax={100} aria-valuenow={meter.pct} aria-valuetext={valueText} data-tone={tone} style={{ "--cu-meter-pct": `${meter.pct}%` } as CSSProperties}>
      <div className="cu-meter-fill" />
    </div>
  );
}

export function Notice({ role, tone, children, ...rest }: { role: "alert" | "status"; tone: Tone; children: ReactNode } & Record<`data-${string}`, string>) {
  return (
    <div className="cu-notice" role={role} data-tone={tone} {...rest}>
      {children}
    </div>
  );
}
