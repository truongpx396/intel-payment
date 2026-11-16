// R120 — the shape of the ports, held by the type checker (credits-ui-ports.md, "Domain types" and invariant 4).
//
// A money figure is an integer, and `number` cannot say so: a float that reaches a balance, a delta or a total is how a
// reconciliation discrepancy gets rounded out of sight of the person best placed to notice it. So every integer-meant field is
// the branded `Int` (`Quantity` is one), and a bare `number` where an integer is meant is a type error, not a convention.
import { expectTypeOf, test } from "vitest";
import type {
  BalanceView,
  BillingAnchor,
  BreakdownSeries,
  CheckoutSource,
  CreditsSnapshot,
  CreditsSource,
  FulfilmentState,
  Int,
  LedgerColumn,
  LedgerRow,
  LimitView,
  PlanCatalog,
  PlanOffer,
  Quantity,
  Scope,
  UnitLabels,
} from "../src/ports.js";

// Every path in T that is typed as a bare `number` (or a union containing one). `unknown` is not a number, and a function's
// parameters are its own business — they are checked where the function is declared. (Exclude, not NonNullable: NonNullable<unknown>
// is `{}`, which a number is assignable to.)
type IsBareNumber<T> = unknown extends T ? false : number extends T ? true : false;
type BareNumbers<T, P extends string = ""> = T extends Int
  ? never
  : T extends (...args: never[]) => unknown
    ? never
    : T extends readonly (infer E)[]
      ? BareNumbers<E, `${P}[]`>
      : T extends object
        ? {
            [K in keyof T & string]-?: IsBareNumber<Exclude<T[K], null | undefined>> extends true ? `${P}.${K}` : BareNumbers<Exclude<T[K], null | undefined>, `${P}.${K}`>;
          }[keyof T & string]
        : never;

test("the walker can see a bare number, or the assertions below prove nothing", () => {
  expectTypeOf<BareNumbers<{ a: { b: number | null }[] }>>().toEqualTypeOf<".a[].b">();
  expectTypeOf<BareNumbers<{ a?: number }>>().toEqualTypeOf<".a">();
  expectTypeOf<BareNumbers<{ a: Int | null; b: unknown; c: string; d: Record<string, unknown> }>>().toEqualTypeOf<never>();
});

test("no field of any money-bearing port is a bare number where an integer is meant", () => {
  expectTypeOf<BareNumbers<CreditsSnapshot>>().toEqualTypeOf<never>();
  expectTypeOf<BareNumbers<PlanCatalog>>().toEqualTypeOf<never>();
  expectTypeOf<BareNumbers<FulfilmentState>>().toEqualTypeOf<never>();
  expectTypeOf<BareNumbers<UnitLabels>>().toEqualTypeOf<never>();
});

test("a plain number is not a Quantity, and a Quantity is still a number", () => {
  // @ts-expect-error 12 is a number, not an Int: it must be made one where it enters (quantity()), which refuses a float
  const plain: Quantity = 12;
  void plain;
  expectTypeOf<Quantity>().toEqualTypeOf<Int>();
  expectTypeOf<Quantity>().toExtend<number>();
});

test("the figures a person reads are Quantities", () => {
  expectTypeOf<LimitView["used"]>().toEqualTypeOf<Quantity>();
  expectTypeOf<LimitView["cap"]>().toEqualTypeOf<Quantity | null>();
  expectTypeOf<LimitView["warnAtPct"]>().toEqualTypeOf<Int>();
  expectTypeOf<BalanceView["remaining"]>().toEqualTypeOf<Quantity>();
  expectTypeOf<BalanceView["granted"]>().toEqualTypeOf<Quantity>();
  expectTypeOf<BalanceView["burnRatePerDay"]>().toEqualTypeOf<Quantity | undefined>();
  expectTypeOf<BreakdownSeries["amount"]>().toEqualTypeOf<Quantity>();
  expectTypeOf<LedgerRow["delta"]>().toEqualTypeOf<Quantity>();
  expectTypeOf<PlanOffer["creditAllotment"]>().toEqualTypeOf<Quantity | null>();
  expectTypeOf<NonNullable<PlanOffer["price"]>["minor"]>().toEqualTypeOf<Int>();
  expectTypeOf<Extract<FulfilmentState, { kind: "granted" }>["amount"]>().toEqualTypeOf<Quantity>();
  expectTypeOf<NonNullable<CreditsSnapshot["series"]>[number]["amount"]>().toEqualTypeOf<Quantity>();
});

test("UnitLabels carries scale and decimals as integers, and formats a Quantity", () => {
  expectTypeOf<UnitLabels>().toHaveProperty("scale");
  expectTypeOf<UnitLabels["scale"]>().toEqualTypeOf<Int | undefined>();
  expectTypeOf<UnitLabels["decimals"]>().toEqualTypeOf<Int | undefined>();
  expectTypeOf<UnitLabels["format"]>().toEqualTypeOf<((q: Quantity) => string) | undefined>();
  expectTypeOf<UnitLabels["unit"]>().toEqualTypeOf<string>();
  expectTypeOf<UnitLabels["one"]>().toEqualTypeOf<string>();
});

test("a balance is described per pool, in draw order, when the realm defines pools", () => {
  expectTypeOf<BalanceView>().toHaveProperty("pools");
  type Pool = NonNullable<BalanceView["pools"]>[number];
  expectTypeOf<Pool["key"]>().toEqualTypeOf<string>();
  expectTypeOf<Pool["label"]>().toEqualTypeOf<string>();
  expectTypeOf<Pool["remaining"]>().toEqualTypeOf<Quantity>();
  expectTypeOf<Pool["appliesTo"]>().toEqualTypeOf<string | undefined>();
});

test("a limit carries no percentage: the fill is derived from used and cap, and a pre-computed one does not type-check", () => {
  const base = { key: "k", label: "L", used: 1 as Quantity, cap: 10 as Quantity, window: "day", warnAtPct: 80 as Int } as const;
  const ok: LimitView = base;
  void ok;
  // @ts-expect-error a LimitView has no `pct`
  const a: LimitView = { ...base, pct: 10 };
  // @ts-expect-error a LimitView has no `fillPct`
  const b: LimitView = { ...base, fillPct: 10 };
  // @ts-expect-error a LimitView has no `percent`
  const c: LimitView = { ...base, percent: 10 };
  void [a, b, c];
});

test("the view's window vocabulary is exactly the six the contract names", () => {
  expectTypeOf<LimitView["window"]>().toEqualTypeOf<"period" | "day" | "hour" | "job" | "call" | "none">();
});

test("a plan offer is a host slug and a price — it has no field a provider's identifier could ride in", () => {
  expectTypeOf<PlanOffer["code"]>().toEqualTypeOf<string>();
  expectTypeOf<PlanOffer["kind"]>().toEqualTypeOf<"subscription" | "one_time">();
  expectTypeOf<Extract<keyof PlanOffer, `${string}${"provider" | "Provider" | "priceId" | "PriceId" | "price_id" | "customer" | "Customer"}${string}`>>().toEqualTypeOf<never>();
  // @ts-expect-error a provider price id has no place on an offer
  const o: PlanOffer = { code: "pro", name: "Pro", kind: "subscription", price: null, creditAllotment: null, providerPriceId: "price_123" };
  void o;
});

test("fulfilment is a state the server reports, and checkout can only hand off", () => {
  expectTypeOf<FulfilmentState["kind"]>().toEqualTypeOf<"none" | "processing" | "granted" | "failed">();
  // begin() resolves to a URL and nothing else: there is no way for it to resolve to "granted"
  expectTypeOf<Awaited<ReturnType<CheckoutSource["begin"]>>>().toEqualTypeOf<{ readonly redirectUrl: string }>();
  expectTypeOf<CheckoutSource["fulfilment"]>().returns.resolves.toEqualTypeOf<FulfilmentState>();
});

test("a ledger column renders to the host's own node type, so ports.ts names no framework", () => {
  expectTypeOf<LedgerColumn["render"]>().returns.toEqualTypeOf<unknown>();
  expectTypeOf<LedgerRow["attributes"]>().toEqualTypeOf<Readonly<Record<string, unknown>>>();
});

test("a purchase permission is data the host supplies, and the billing anchor is optional", () => {
  expectTypeOf<PlanCatalog["canPurchase"]>().toEqualTypeOf<boolean>();
  expectTypeOf<CreditsSnapshot["anchor"]>().toEqualTypeOf<BillingAnchor | undefined>();
});

test("a source loads a snapshot for a scope, and may push", () => {
  expectTypeOf<CreditsSource["load"]>().toEqualTypeOf<(scope: Scope) => Promise<CreditsSnapshot>>();
  expectTypeOf<Scope["realm"]>().toEqualTypeOf<string | undefined>();
});
