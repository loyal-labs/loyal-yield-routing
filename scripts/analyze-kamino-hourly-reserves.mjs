const API_BASE = "https://api.kamino.finance";
const JUPITER_QUOTE_API = "https://api.jup.ag/swap/v1/quote";
const ENV = "mainnet-beta";
const MARCH_START = "2026-03-01";
const EARLIEST_FALLBACK_START = "2025-01-01";
const END_DATE = "2026-05-19";
const FREQUENCY = "hour";
const STARTING_VALUE = 1000;
const TVL_FLOOR = 100_000;
const APY_CAP = 0.5;
const POOL_CHANGE_LAMPORTS = 5_000;
const SOL_PRICE_USD = 84.82;
const POOL_CHANGE_USD = (POOL_CHANGE_LAMPORTS / 1_000_000_000) * SOL_PRICE_USD;
const OUTPUT_PATH = "data/kamino-hourly-reserve-analysis.json";
const DATA_CACHE_PATH = "data/kamino-hourly-reserve-history-cache.json";
const QUOTE_CACHE_PATH = "data/kamino-hourly-jupiter-quote-cache.json";
const DEFAULT_CACHE_MAX_AGE_HOURS = 24;
const FETCH_TIMEOUT_MS = 20_000;
const STABLE_SYMBOLS = new Set([
  "AUSD",
  "CASH",
  "EUSX",
  "FDUSD",
  "PYUSD",
  "SUSD",
  "SUSDE",
  "SYRUPUSDC",
  "USCC",
  "USDC",
  "USDCDEP",
  "USDE",
  "USD1",
  "USDG",
  "USDH",
  "USDS",
  "USDT",
  "USDY",
]);

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function parseArgs(args) {
  const options = {
    refresh: false,
    cacheOnly: false,
    maxAgeHours: DEFAULT_CACHE_MAX_AGE_HOURS,
  };
  for (const arg of args) {
    if (arg === "--refresh") options.refresh = true;
    if (arg === "--cache-only" || arg === "--no-fetch") options.cacheOnly = true;
    if (arg.startsWith("--max-age-hours=")) {
      const value = Number(arg.split("=")[1]);
      if (Number.isFinite(value) && value >= 0) options.maxAgeHours = value;
    }
  }
  if (options.refresh && options.cacheOnly) {
    throw new Error("Use either --refresh or --cache-only, not both.");
  }
  return options;
}

async function readJsonFile(path) {
  const file = Bun.file(path);
  if (!(await file.exists())) return null;
  return file.json();
}

function cacheAgeHours(cache) {
  const generatedAt = new Date(cache?.generatedAt ?? 0).getTime();
  if (!Number.isFinite(generatedAt) || generatedAt <= 0) return Infinity;
  return (Date.now() - generatedAt) / (60 * 60 * 1000);
}

function cacheParamsMatch(cache) {
  const assumptions = cache?.assumptions ?? {};
  return (
    assumptions.env === ENV &&
    assumptions.requestedStart === MARCH_START &&
    assumptions.earliestFallbackStart === EARLIEST_FALLBACK_START &&
    assumptions.requestedEnd === END_DATE &&
    assumptions.frequency === FREQUENCY
  );
}

function isFreshCache(cache, maxAgeHours) {
  return cacheParamsMatch(cache) && cache?.complete === true && cacheAgeHours(cache) <= maxAgeHours;
}

function isFreshQuoteCache(cache, maxAgeHours) {
  return cacheParamsMatch(cache) && cacheAgeHours(cache) <= maxAgeHours;
}

function cacheAssumptions() {
  return {
    env: ENV,
    requestedStart: MARCH_START,
    earliestFallbackStart: EARLIEST_FALLBACK_START,
    requestedEnd: END_DATE,
    frequency: FREQUENCY,
  };
}

function formatDuration(ms) {
  if (!Number.isFinite(ms) || ms < 0) return "unknown";
  const totalSeconds = Math.round(ms / 1000);
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (hours > 0) return `${hours}h ${minutes}m ${seconds}s`;
  if (minutes > 0) return `${minutes}m ${seconds}s`;
  return `${seconds}s`;
}

function progressLine({ phase, index, total, startedAt, label }) {
  const elapsedMs = Date.now() - startedAt;
  const averageMs = index > 0 ? elapsedMs / index : 0;
  const remaining = Math.max(total - index, 0);
  const etaMs = averageMs * remaining;
  const percent = total > 0 ? ((index / total) * 100).toFixed(1) : "100.0";
  return [
    `[${phase}] ${index}/${total} (${percent}%)`,
    `elapsed ${formatDuration(elapsedMs)}`,
    `eta ${formatDuration(etaMs)}`,
    label,
  ].filter(Boolean).join(" | ");
}

async function getJson(url, retries = 4) {
  let lastError;
  for (let attempt = 0; attempt <= retries; attempt += 1) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);
    try {
      const response = await fetch(url, {
        signal: controller.signal,
        headers: {
          accept: "application/json",
          "user-agent": "loyal-yield-routing-analysis/1.0",
        },
      });
      clearTimeout(timeout);
      if (response.status === 429 || response.status >= 500) {
        throw new Error(`${response.status} ${response.statusText}`);
      }
      if (!response.ok) {
        const text = await response.text();
        throw new Error(`${response.status} ${response.statusText}: ${text.slice(0, 200)}`);
      }
      return response.json();
    } catch (error) {
      clearTimeout(timeout);
      lastError = error;
      if (attempt < retries) {
        await sleep(300 * 2 ** attempt);
      }
    }
  }
  throw lastError;
}

function marketUrl(path, params = {}) {
  const url = new URL(path, API_BASE);
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null) {
      url.searchParams.set(key, value);
    }
  }
  return url.toString();
}

async function fetchMarkets() {
  return getJson(`${API_BASE}/v2/kamino-market`);
}

async function fetchReserveAddresses(market) {
  const url = marketUrl("/kamino-market/reserves/account-data", {
    markets: market.lendingMarket,
  });
  const payload = await getJson(url);
  return (payload[0]?.reserves ?? []).map((reserve) => reserve.pubkey);
}

async function fetchHistory(marketAddress, reserveAddress, startDate) {
  const url = marketUrl(
    `/kamino-market/${marketAddress}/reserves/${reserveAddress}/metrics/history`,
    {
      env: ENV,
      start: startDate,
      end: END_DATE,
      frequency: FREQUENCY,
    },
  );
  return getJson(url);
}

function latestEntry(history) {
  return history?.history?.at(-1) ?? null;
}

function isStableMetric(metrics) {
  const symbol = String(metrics?.symbol ?? "").toUpperCase();
  const normalizedSymbol = symbol.replace(/[^A-Z0-9]/g, "");
  const price = Number(metrics?.assetOraclePriceUSD ?? metrics?.assetPriceUSD);
  const symbolLooksStable = STABLE_SYMBOLS.has(normalizedSymbol);
  return symbolLooksStable && Number.isFinite(price) && price >= 0.75 && price <= 1.35;
}

function parsePoint(market, reserve, item) {
  const metrics = item.metrics ?? {};
  return {
    timestamp: item.timestamp,
    marketAddress: market.lendingMarket,
    marketName: market.name,
    reserveAddress: reserve.reserveAddress,
    symbol: metrics.symbol,
    mintAddress: metrics.mintAddress,
    decimals: Number(metrics.decimals ?? 6),
    supplyApy: Number(metrics.supplyInterestAPY),
    depositTvl: Number(metrics.depositTvl),
    assetOraclePriceUsd: Number(metrics.assetOraclePriceUSD ?? metrics.assetPriceUSD),
  };
}

function isEligiblePoint(point) {
  return (
    Number.isFinite(point.supplyApy) &&
    point.supplyApy >= 0 &&
    point.supplyApy < APY_CAP &&
    Number.isFinite(point.depositTvl) &&
    point.depositTvl > TVL_FLOOR
  );
}

function buildHourlyChoices(reserves) {
  const byTimestamp = new Map();
  for (const reserve of reserves) {
    for (const item of reserve.history.history ?? []) {
      const point = parsePoint(reserve.market, reserve, item);
      if (!isEligiblePoint(point)) continue;
      const choices = byTimestamp.get(point.timestamp) ?? [];
      choices.push(point);
      byTimestamp.set(point.timestamp, choices);
    }
  }
  return [...byTimestamp.entries()]
    .sort(([a], [b]) => new Date(a) - new Date(b))
    .map(([timestamp, choices]) => ({
      timestamp,
      choices: choices.sort((a, b) => b.supplyApy - a.supplyApy),
    }));
}

function directedPairKey(from, to) {
  return `${from.mintAddress}->${to.mintAddress}`;
}

function reserveKey(point) {
  return `${point.marketAddress}:${point.reserveAddress}`;
}

function tokenKey(point) {
  return point.mintAddress;
}

async function fetchJupiterCost(from, to) {
  if (tokenKey(from) === tokenKey(to)) {
    return { available: true, lossFraction: 0, samples: [] };
  }

  const fromPrice = Number.isFinite(from.assetOraclePriceUsd) && from.assetOraclePriceUsd > 0
    ? from.assetOraclePriceUsd
    : 1;
  const toPrice = Number.isFinite(to.assetOraclePriceUsd) && to.assetOraclePriceUsd > 0
    ? to.assetOraclePriceUsd
    : 1;
  const amount = BigInt(Math.round((STARTING_VALUE / fromPrice) * 10 ** from.decimals));
  const samples = [];
  for (let i = 0; i < 3; i += 1) {
    const url = new URL(JUPITER_QUOTE_API);
    url.searchParams.set("inputMint", from.mintAddress);
    url.searchParams.set("outputMint", to.mintAddress);
    url.searchParams.set("amount", amount.toString());
    url.searchParams.set("slippageBps", "50");
    url.searchParams.set("swapMode", "ExactIn");
    try {
      const quote = await getJson(url.toString(), 2);
      const inUi = Number(quote.inAmount) / 10 ** from.decimals;
      const outUi = Number(quote.outAmount) / 10 ** to.decimals;
      if (Number.isFinite(inUi) && Number.isFinite(outUi) && inUi > 0 && outUi > 0) {
        samples.push(Math.max(0, 1 - (outUi * toPrice) / (inUi * fromPrice)));
      }
    } catch {
      // Missing routes and public API throttles are treated as unavailable.
    }
    await sleep(250);
  }
  if (samples.length === 0) {
    return { available: false, lossFraction: null, samples: [] };
  }
  samples.sort((a, b) => a - b);
  return {
    available: true,
    lossFraction: samples[Math.floor(samples.length / 2)],
    samples,
  };
}

async function buildJupiterCosts(hourlyChoices, options) {
  const pairs = new Map();
  const representatives = new Map();
  for (const hour of hourlyChoices) {
    for (const choice of hour.choices) {
      if (!representatives.has(tokenKey(choice))) {
        representatives.set(tokenKey(choice), choice);
      }
    }
  }

  const tokens = [...representatives.values()];
  for (const from of tokens) {
    for (const to of tokens) {
      if (tokenKey(from) === tokenKey(to)) continue;
      pairs.set(directedPairKey(from, to), { from, to });
    }
  }

  const quoteCache = await readJsonFile(QUOTE_CACHE_PATH);
  const cachedCosts = isFreshQuoteCache(quoteCache, options.maxAgeHours)
    ? quoteCache.jupiterCosts ?? {}
    : {};
  const costs = { ...cachedCosts };
  const startedAt = Date.now();
  let processed = 0;
  const total = pairs.size;
  for (const [key, pair] of pairs) {
    processed += 1;
    if (!options.refresh && costs[key]) {
      console.log(progressLine({
        phase: "jupiter",
        index: processed,
        total,
        startedAt,
        label: `cached ${key}`,
      }));
      continue;
    }
    if (options.cacheOnly) {
      costs[key] = { available: false, lossFraction: null, samples: [] };
      console.log(progressLine({
        phase: "jupiter",
        index: processed,
        total,
        startedAt,
        label: `cache-only miss ${key}`,
      }));
      continue;
    }
    console.log(progressLine({
      phase: "jupiter",
      index: processed,
      total,
      startedAt,
      label: `fetch ${key}`,
    }));
    costs[key] = await fetchJupiterCost(pair.from, pair.to);
    await Bun.write(QUOTE_CACHE_PATH, JSON.stringify({
      generatedAt: new Date().toISOString(),
      assumptions: cacheAssumptions(),
      jupiterQuoteApi: JUPITER_QUOTE_API,
      jupiterCosts: costs,
    }, null, 2));
  }

  await Bun.write(QUOTE_CACHE_PATH, JSON.stringify({
    generatedAt: new Date().toISOString(),
    assumptions: cacheAssumptions(),
    jupiterQuoteApi: JUPITER_QUOTE_API,
    jupiterCosts: costs,
  }, null, 2));
  return costs;
}

function emptyKaminoCache() {
  return {
    generatedAt: new Date().toISOString(),
    complete: false,
    assumptions: cacheAssumptions(),
    markets: [],
    marketReserves: [],
    reserveHistories: [],
  };
}

async function writeKaminoCache(cache) {
  cache.generatedAt = new Date().toISOString();
  await Bun.write(DATA_CACHE_PATH, JSON.stringify(cache, null, 2));
}

function transitionCost(value, from, to, costs) {
  if (reserveKey(from) === reserveKey(to)) return 0;
  const quoteCost = tokenKey(from) === tokenKey(to)
    ? { available: true, lossFraction: 0 }
    : costs[directedPairKey(from, to)];
  if (!quoteCost?.available) return null;
  return value * (quoteCost.lossFraction ?? 0) + POOL_CHANGE_USD;
}

function simulate(hourlyChoices, costs) {
  let states = new Map();
  const backpointers = [];
  for (const choice of hourlyChoices[0].choices) {
    states.set(reserveKey(choice), {
      value: STARTING_VALUE,
      point: choice,
      prevKey: null,
      switchCost: 0,
    });
  }

  let poolChanges = 0;
  let blockedByQuote = 0;

  for (let i = 1; i < hourlyChoices.length; i += 1) {
    const previousTimestamp = hourlyChoices[i - 1].timestamp;
    const timestamp = hourlyChoices[i].timestamp;
    const elapsedYears =
      (new Date(timestamp).getTime() - new Date(previousTimestamp).getTime()) /
      (365 * 24 * 60 * 60 * 1000);

    const nextStates = new Map();
    const previousStates = [...states.entries()];
    for (const candidate of hourlyChoices[i].choices) {
      let best = null;
      for (const [fromKey, state] of previousStates) {
        const accruedValue = state.value * Math.exp(state.point.supplyApy * elapsedYears);
        const switchCost = transitionCost(accruedValue, state.point, candidate, costs);
        if (switchCost === null) {
          blockedByQuote += 1;
          continue;
        }
        const candidateValue = accruedValue - switchCost;
        if (!best || candidateValue > best.value) {
          best = {
            value: candidateValue,
            point: candidate,
            prevKey: fromKey,
            switchCost,
          };
        }
      }
      if (best) nextStates.set(reserveKey(candidate), best);
    }
    states = nextStates;
    backpointers.push(states);
  }

  let bestState = null;
  let bestKey = null;
  for (const [key, state] of states) {
    if (!bestState || state.value > bestState.value) {
      bestState = state;
      bestKey = key;
    }
  }

  const path = [];
  for (let i = backpointers.length - 1; i >= 0; i -= 1) {
    const state = backpointers[i].get(bestKey);
    if (!state) break;
    const timestamp = hourlyChoices[i + 1].timestamp;
    const previousKey = state.prevKey;
    if (previousKey !== bestKey) {
      path.push({ timestamp, ...state.point, switchCost: state.switchCost });
      poolChanges += 1;
    }
    bestKey = previousKey;
  }
  const firstChoice = hourlyChoices[0].choices.find((choice) => reserveKey(choice) === bestKey)
    ?? hourlyChoices[0].choices[0];
  path.push({ timestamp: hourlyChoices[0].timestamp, ...firstChoice, switchCost: 0 });
  path.reverse();

  const value = bestState.value;
  const start = new Date(hourlyChoices[0].timestamp);
  const end = new Date(hourlyChoices.at(-1).timestamp);
  const elapsedYears = (end.getTime() - start.getTime()) / (365 * 24 * 60 * 60 * 1000);
  const annualizedApy = Math.exp(Math.log(value / STARTING_VALUE) / elapsedYears) - 1;

  return {
    start: start.toISOString(),
    end: end.toISOString(),
    decisions: hourlyChoices.length - 1,
    startingValue: STARTING_VALUE,
    endingValue: value,
    profit: value - STARTING_VALUE,
    annualizedApy,
    poolChanges,
    blockedByQuote,
    path,
  };
}

async function fetchKaminoData() {
  const existing = await readJsonFile(DATA_CACHE_PATH);
  const canResume = existing
    && existing.assumptions?.env === ENV
    && existing.assumptions?.requestedStart === MARCH_START
    && existing.assumptions?.earliestFallbackStart === EARLIEST_FALLBACK_START
    && existing.assumptions?.requestedEnd === END_DATE
    && existing.assumptions?.frequency === FREQUENCY;
  const cache = canResume ? existing : emptyKaminoCache();
  cache.complete = false;
  const markets = await fetchMarkets();
  cache.markets = markets;
  cache.marketReserves ??= [];
  cache.reserveHistories ??= [];
  await writeKaminoCache(cache);

  const marketStartedAt = Date.now();
  for (const [marketIndex, market] of markets.entries()) {
    const existingForMarket = cache.marketReserves.filter(
      (reserve) => reserve.market.lendingMarket === market.lendingMarket,
    );
    if (existingForMarket.length > 0) {
      console.log(progressLine({
        phase: "markets",
        index: marketIndex + 1,
        total: markets.length,
        startedAt: marketStartedAt,
        label: `${market.name}: ${existingForMarket.length} cached reserves`,
      }));
      continue;
    }
    const addresses = await fetchReserveAddresses(market);
    for (const reserveAddress of addresses) {
      cache.marketReserves.push({ market, reserveAddress });
    }
    console.log(progressLine({
      phase: "markets",
      index: marketIndex + 1,
      total: markets.length,
      startedAt: marketStartedAt,
      label: `${market.name}: ${addresses.length} reserves`,
    }));
    await writeKaminoCache(cache);
    await sleep(100);
  }

  const seenHistory = new Set(
    cache.reserveHistories.map((reserve) => `${reserve.market.lendingMarket}:${reserve.reserveAddress}`),
  );
  const historyStartedAt = Date.now();
  for (const [reserveIndex, reserve] of cache.marketReserves.entries()) {
    const historyKey = `${reserve.market.lendingMarket}:${reserve.reserveAddress}`;
    if (seenHistory.has(historyKey)) {
      console.log(progressLine({
        phase: "histories",
        index: reserveIndex + 1,
        total: cache.marketReserves.length,
        startedAt: historyStartedAt,
        label: `cached ${reserve.market.name} ${reserve.reserveAddress}`,
      }));
      continue;
    }

    let history;
    try {
      console.log(progressLine({
        phase: "histories",
        index: reserveIndex + 1,
        total: cache.marketReserves.length,
        startedAt: historyStartedAt,
        label: `fetch ${reserve.market.name} ${reserve.reserveAddress}`,
      }));
      history = await fetchHistory(reserve.market.lendingMarket, reserve.reserveAddress, MARCH_START);
      if (!history.history?.length) {
        history = await fetchHistory(
          reserve.market.lendingMarket,
          reserve.reserveAddress,
          EARLIEST_FALLBACK_START,
        );
      }
    } catch (error) {
      console.log(`skip ${reserve.market.name} ${reserve.reserveAddress}: ${error.message}`);
      continue;
    }

    cache.reserveHistories.push({ ...reserve, history });
    seenHistory.add(historyKey);
    await writeKaminoCache(cache);
    await sleep(120);
  }

  cache.complete = true;
  await writeKaminoCache(cache);
  return cache;
}

async function loadKaminoData(options) {
  const cache = await readJsonFile(DATA_CACHE_PATH);
  if (!options.refresh && isFreshCache(cache, options.maxAgeHours)) {
    console.log(
      `using cached Kamino data from ${cache.generatedAt} (${cacheAgeHours(cache).toFixed(2)}h old)`,
    );
    return cache;
  }

  if (options.cacheOnly) {
    if (!cache) throw new Error(`Missing Kamino cache at ${DATA_CACHE_PATH}. Run without --cache-only once.`);
    if (cache.complete !== true) {
      throw new Error(`Kamino cache at ${DATA_CACHE_PATH} is incomplete. Run --refresh to resume it.`);
    }
    console.log(
      `using stale cached Kamino data from ${cache.generatedAt} because --cache-only was set`,
    );
    return cache;
  }

  if (cache) {
    console.log(
      `refreshing Kamino data; cache is ${cacheAgeHours(cache).toFixed(2)}h old or parameters changed`,
    );
  }
  return fetchKaminoData();
}

function filterStableReserves(reserveHistories) {
  const stableReserves = [];
  for (const reserve of reserveHistories) {
    const { history } = reserve;
    const latest = latestEntry(history);
    if (latest && isStableMetric(latest.metrics)) {
      stableReserves.push({ ...reserve, history });
      console.log(
        `stable ${reserve.market.name} ${latest.metrics.symbol} ${reserve.reserveAddress}: ${history.history.length} points`,
      );
    }
  }
  return stableReserves;
}

async function main() {
  const options = parseArgs(process.argv.slice(2));
  await Bun.write(OUTPUT_PATH, "{}");

  const kaminoData = await loadKaminoData(options);
  const { markets, marketReserves, reserveHistories } = kaminoData;
  const stableReserves = filterStableReserves(reserveHistories);

  const hourlyChoices = buildHourlyChoices(stableReserves);
  const costs = await buildJupiterCosts(hourlyChoices, options);
  const result = simulate(hourlyChoices, costs);

  const report = {
    generatedAt: new Date().toISOString(),
    api: {
      markets: `${API_BASE}/v2/kamino-market`,
      reserves: `${API_BASE}/kamino-market/reserves/account-data`,
      history: `${API_BASE}/kamino-market/{market}/reserves/{reserve}/metrics/history`,
      jupiterQuote: JUPITER_QUOTE_API,
    },
    assumptions: {
      ...cacheAssumptions(),
      startingValue: STARTING_VALUE,
      tvlFloor: TVL_FLOOR,
      apyCap: APY_CAP,
      poolChangeLamports: POOL_CHANGE_LAMPORTS,
      solPriceUsd: SOL_PRICE_USD,
      poolChangeUsd: POOL_CHANGE_USD,
    },
    counts: {
      markets: markets.length,
      reservesSeen: marketReserves.length,
      reserveHistories: reserveHistories.length,
      stableReserves: stableReserves.length,
      eligibleHourlySnapshots: hourlyChoices.length,
    },
    stableReserves: stableReserves.map((reserve) => {
      const first = reserve.history.history[0];
      const last = reserve.history.history.at(-1);
      return {
        marketName: reserve.market.name,
        marketAddress: reserve.market.lendingMarket,
        reserveAddress: reserve.reserveAddress,
        symbol: last.metrics.symbol,
        mintAddress: last.metrics.mintAddress,
        firstTimestamp: first.timestamp,
        lastTimestamp: last.timestamp,
        points: reserve.history.history.length,
        latestDepositTvl: Number(last.metrics.depositTvl),
        latestSupplyApy: Number(last.metrics.supplyInterestAPY),
      };
    }),
    jupiterCosts: costs,
    result,
  };

  await Bun.write(OUTPUT_PATH, JSON.stringify(report, null, 2));
  console.log(JSON.stringify({
    output: OUTPUT_PATH,
    counts: report.counts,
    result: {
      start: result.start,
      end: result.end,
      endingValue: result.endingValue,
      annualizedApy: result.annualizedApy,
      poolChanges: result.poolChanges,
    },
  }, null, 2));
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
