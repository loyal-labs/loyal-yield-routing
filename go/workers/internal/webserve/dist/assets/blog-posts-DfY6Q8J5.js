var e=`---
title: "(Almost?) PMF, a roadmap update, and what's next"
date: "2026-07-18"
hero: hero.jpg
description: "Seeker Summer treated us well."
author:
  name: "Rodion, founder"
---
Seeker Summer treated us well.

Over the past few weeks, 9,000 Seekers installed Loyal, and more than 3,000 launched an agent. Then did the thing we were quietly hoping for and trusted those agents with real money. Loyal Earn holds $390,000 in USDC today. The feature is less than a month available for public.

It's too early to talk about retention, and installs are just installs. But strangers depositing six figures into a month-old feature is the strongest signal we've had since we started building. So let's talk about what got us here, and where the business goes from here.

## The two bets

The last few quarters we built the foundation: the wallet, private transfers, Smart Accounts with policies, the Chrome extension. Necessary work, but foundations don't make a quarter. Two bets did.

**Bet one: ship on Seeker.** A phone full of crypto-native users who actually try new dApps is the best distribution we could ask for. We wanted to be there day one, so we cut scope elsewhere and made the deadline. The install and activation numbers above are the result.

**Bet two: ship Earn.** Most people don't want to run five DeFi tabs to make idle stablecoins work. They want one button that makes their money bigger. That's what a Smart Account with an agent inside is for. Deposit USDC, the agent hunts the yield, you stay in control of the guardrails.

Both bets were versions of the same thesis: agents are becoming the interface for finance, and the missing piece is an execution layer people can actually trust with money. The $390K sitting in Earn says the thesis holds.

## Where the revenue comes from

Here's how Earn works today. You deposit USDC. An Earn agent watches lending markets and rebalances your deposit between pools of the same mint, chasing the best rate. USDC stays USDC the whole time.

The next step is cross-mint rebalancing. Stablecoins from solid issuers trade 1:1 basically always. So when the APY on one of them spikes, like the recent 20% spike on PYUSD, the right move is obvious: swap in, farm the elevated yield, swap back when it normalizes. Same risk profile, better rate. And when the agent executes that swap inside Loyal, the routing fee stays in the product instead of going to an external router.

Some napkin math on what this becomes. Mature yield protocols retain roughly 0.5 to 1% of TVL per year as protocol revenue (DeFiLlama numbers, directional). At that ratio, every dollar in Earn is recurring revenue. We're at $390K after month one, before cross-mint swap fees, and before the services below. You can do the multiplication.

## Failsafe

On April 1, Drift was drained for $285 million. Everyone quotes the 12-minute number, but that was only the first burst. The full drain ran about two and a half hours. And the real warning came five days earlier: on March 27, Drift's security council migrated to a new 2-of-5 multisig with a zero-second timelock, with four of the five signers brand new. That red flag sat on-chain, in public, for five days. Nobody acted on it, because no human watches authority configs around the clock.

An agent can.

Failsafe is a watchdog agent for your Smart Account with exactly one permission: pull your funds back to safety. It watches pool health, depeg signals, authority upgrades, timelock changes, the whole class of signs that precede an exploit. First sign of trouble, it exits your position. It cannot trade, cannot send funds anywhere else, cannot do anything but bring your money home. That's the point of policies.

We start building Failsafe in August, as a paid layer on top of Earn.

## Quests

Earn works without holding anything, and that stays true. But we're designing a progression system on top: higher APY tiers and extra features you earn by doing things in the app or by staking [$LOYAL](https://x.com/search?q=%24LOYAL&src=cashtag_click). Alongside it, a referral program that pays you a share of the yield from the deposits you bring in.

Designing now, ships in Q3. We'll publish the exact tiers and numbers once they're final.

## One housekeeping note

The roadmap is moving off [askloyal.com](https://askloyal.com/) and into the docs because the site's job has changed. People landing there now are users deciding whether to deposit, and they need to see what Loyal does today. The full roadmap will live now at [docs.askloyal.com](https://docs.askloyal.com/) and gets updated as we ship.

## Monday

Also, on Monday we're putting a new proposal in front of the DAO. See you then.

Stay Loyal.

Website: [https://askloyal.com](https://askloyal.com/)  
Docs: [https://docs.askloyal.com](https://docs.askloyal.com/)  
Wallet (Telegram): [https://t.me/askloyal_tgbot](https://t.me/askloyal_tgbot)  
Chrome extension: [https://chromewebstore.google.com/detail/cdienfadefhlaknmedckgifkjdbioack](https://chromewebstore.google.com/detail/cdienfadefhlaknmedckgifkjdbioack)  
Community: [https://t.me/loyal_tgchat](https://t.me/loyal_tgchat)  
X: [https://x.com/loyal_hq](https://x.com/loyal_hq)  
Discord: [https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)  
GitHub: [https://github.com/loyal-labs](https://github.com/loyal-labs)  
[$LOYAL](https://x.com/search?q=%24LOYAL&src=cashtag_click): [https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)
`,t=`---
title: "Ginormous June"
date: "2026-07-08"
hero: hero.jpg
description: "Our sole focus in June was growing our user and financial metrics."
author:
  name: "Chris, founder"
  avatar: /blog/authors/chris.png
---
- In just two weeks, we've secured over $200K in deposits. $27K of those deposits came from balance sweep features. We've routed over $2.3M in volume.
- The main growth drivers are our earn routing and balance sweep features released in mid-June. Right now, it's the best low-risk APY on Solana.
- Baseline goal for July: $300K in TVL. Track our progress at [stats.askloyal.com](https://stats.askloyal.com/)!

Our sole focus in June was growing our user and financial metrics. We leveraged the distribution we built through April and May to offer the best low-risk stablecoin APY on Solana to every loyal user.

In this article, we'll go over how the magic behind our product works, things that users love about it, and tease what's coming in July.

![Red title card reading "EARN ROUTING GOES LIVE" with the Loyal logo and askloyal.com](img-1.jpg)

[Watch the clip on X](https://x.com/loyal_hq/status/2069918828558266525)

## $200K TVL - How and Why

We built a product that users love and our numbers back it up. Let us explain how it works and why everyone who tried it fell in love with it.

**Let's start with how it works**:

1. You have a Squads smart account. You create permissions that allow your account to sign transactions as long as the transactions satisfy the specified constraints.
2. For earn routing, you approve moving your money through the six biggest Kamino reserves.
3. For balance sweep, you approve your smart account to move anything above the threshold from your wallet balance to your smart account.

Earn routing works on top of Squads and Kamino. Both protocols are formally verified and have undergone multiple audits. We introduce zero additional protocol dependencies. As long as the user trusts Squads and Kamino, we introduce no additional protocol risk. Same goes for our balance sweep: it uses Squads and new Solana subscription primitive, with zero new dependencies added.

**Here's why users love it**:

1. It works with hardware wallets, too. Ledger users have deposited over $80,000. After the initial setup, everything is done for you. It really feels like magic. We're so proud of it!
2. It's the best low-risk stablecoin APY on Solana. We're the best at capturing lending yield arbitrage across Kamino reserves. Users won't chase a 0.1% APY difference. We will. At scale, the compounding efficiencies give incredible results.
3. It's 100% open source and onchain. If our website goes down tomorrow, you'll still be able to withdraw your money. If you don't trust us, you can [run everything yourself locally](https://github.com/loyal-labs/loyal-yield-routing) on your machine, too.

This is the first step toward our vision of agentic finance on Solana: automations that never require users to compromise on security or self-custody. With Loyal Earn, you connect your wallet once, set up permissions for your smart account and then watch your money grow!

> guess this is what happens when you build on top of Kamino.
>
> @loyal_hq is moving funds across @kamino markets, hunting APY spikes as they appear and currently earning ~10% APY.
>
> less rate volatility for borrowers, higher APY for lenders.
>
> based.
>
> — [Kamino Intern (@kaminointern), Jul 1](https://x.com/kaminointern/status/2072395107366531580)

## Acceleration in July

July will be intense. Three priorities for the month:

1. Our baseline goal is to cross $300K TVL. It's an important milestone to start having integration conversations with neobanks, wallets, apps, and protocols.
2. Seeker Summer launch. Loyal is the app opening this year's Solana Mobile event. This will allow us to see how the system scales with hundreds of users. It's crucial for integrations.

![Seeker Summer title card over retro footage of a hand holding an early mobile phone](img-2.jpg)

[Watch the clip on X](https://x.com/solanamobile/status/2074540516541345927)

3. Our proposal goes live within the next 14 days. It'll cover creation of the first autonomous vault. It'll have the same investor protections as our main Loyal MetaDAO treasury, but it'd be able to move money between Kamino and deposit liquidity into Meteora pools. The proposal will also cover OTC and move us to a new incentives program. More on that later this week.

Among other things, July will see a big announcement we're planning with [@mywebacy](https://x.com/mywebacy). We'll also be looking to add support for USDT, CASH and PYUSD for our earn routing.

## Final Thoughts

Loyal would be nothing without you, our dear reader.

We are a small team with no big names backing us.  
There's no liquid fund to bail us out if we fail.  
Winning is our only option.

Every like, reply and retweet matters.  
It makes a difference.  
You make a difference.

Please keep challenging us.  
Please keep asking questions.  
Please give us all your product suggestions.

Join [our community chat](https://t.me/loyal_tgchat), or just text [Chris](https://x.com/candyflipline) or [Rodion](https://x.com/space_symmetry). We value you and your voice.

Keep it coming, boys!

Loyal always,  
Chris

---

Website — [https://askloyal.com](https://askloyal.com/)  
Docs — [https://docs.askloyal.com](https://docs.askloyal.com/)  
Buy [$LOYAL](https://x.com/search?q=%24LOYAL&src=cashtag_click) on Jupiter — [https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)  
Telegram Community — [https://t.me/loyal_tgchat](https://t.me/loyal_tgchat)  
Discord — [https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)  
X — [https://x.com/loyal_hq](https://x.com/loyal_hq)  
GitHub — [https://github.com/loyal-labs](https://github.com/loyal-labs)
`,n=`---
title: "How Loyal uses Nillion for user-owned data storage"
date: "2025-11-07"
hero: hero.png
description: "When we see private AI, we see promises of shredded logs or “prompts deleted after use”."
author:
  name: "Rodion, founder"
---
When we see private AI, we see promises of shredded logs or “prompts deleted after use”. However, if we want to reach truly private intelligence the very architecture of the network should make the data impossible to be viewed or repurposed by anyone except the user. That’s what Loyal is building: a private intelligence network where you retain the ownership of the data.

One of the key tech here solutions is nilDB, Nillion’s encrypted, secret-sharing database module. Loyal uses nilDB so that user data and AI interactions are encrypted end to end and only you decrypt and use them. Neither Loyal nor Nillion can access the data by design.

## The problem we address

Most AI assistants, chatbots or “intelligence” layers treat user history like logs. Prompts, responses and model memory are stored the same way as usual. That means exposure to training reuse, subpoena risk and especially platform lock-in which is so common that we don’t even question it.

But what if your chat history and memory can be stored independently from the service you use and transferred between different models and agents, while not being loggable by the provider or third parties? What if that memory is portable, revocable, and under your wallet’s control? That’s the architecture we pursue.

## Loyal’s architecture overview

Loyal routes each user request into a chain-anchored session: your wallet ties you to a session via a Solana PDA (Program Derived Address). Each request executes inside an attested confidential VM, so the inference can’t be tampered with, and logs can’t be extracted by the compute provider. When it comes to storing that data, instead of writing everything to a central database, Loyal writes the prompt and response into a decentralized storage controlled by the user's Solana wallet.

**For the end user this architecture allows:**

- Full ownership of the chat history/memory.

- Compute provider/agent can’t extract your logs and sensitive data.

- Seamlessly transfer the history between different models and compute providers

- Transfer the history between different apps/UIs that use Loyal’s backend

## Nillion nilDB

Nillion’s mission is to decentralise trust for sensitive data in the same way blockchains decentralised transactions.  Its “Blind Modules” make it possible to store and compute on data while keeping it encrypted (or split into secret-shares) so that no single node or service has full access.

nilDB is the private storage module within this stack: a structured data persistence service where you can define collections with schemas, mark sensitive fields, encrypt them end-to-end or secret-share them across nodes, and retain analytic or query capabilities without exposing it in plaintext. Loyal’s implementation derives encryption keys from the user's solana wallet and the backend never holds them.

## How Loyal uses nilDB: request → response → user-controlled memory

Let’s follow a typical interaction inside Loyal’s stack:

A user sends a request (e.g., “What’s my next quarter’s hiring plan?”). Loyal writes that request into nilDB. Because fields may include sensitive metadata (user ID, session context, business specifics), those go into the user-owned-collection.

The request is forwarded through Loyal’s TEE-anchored inference path. The model returns an answer. Loyal writes the answer into nilDB too. Sensitive parts again go into the encrypted user-owned fields. The full transcript is saved but under encryption and user control.

At no point is the full plaintext transcript held in a central unencrypted log. The data remains privately yours, even though it’s stored and can be searched or used for context. Because Loyal writes both prompt and response into nilDB, your agent can build memory over time — but you remain the gate-keeper.

## Developer and implementation angle

From a builder’s perspective, the stack looks like this: define a collection in nilDB with a JSON schema, mark which fields are sensitive (encrypted / secret-share) and which can be plain (non-sensitive metadata). Use Nillion’s REST API or SDK to write data: authenticate via DID-JWT, issue writes and queries.

The user’s decryption key resides with them (derived from wallet private key), so Loyal cannot view the encrypted fields. Loyal orchestrates the inference, but the memory store sits on nilDB in a way that respects user ownership. When a session ends, the user can export or delete their data.

## Why we chose to go this way

There are three big implications. First: True data ownership. The user owns their memory and conversations and the provider cannot introspect or reuse them unilaterally. Second: Privacy by design. Rather than a promise (“we won’t look”), the architecture makes it impossible for logs to exist in plain form. Third: Intelligence with context. Because Loyal preserves memory in nilDB, your agent can get smarter over time and you get the benefit of continuity and personalization, but not the risk of unmanaged logs.

## Conclusion

If you believe that intelligence should serve you, not log you, then Loyal’s architecture built on nilDB is worth watching closely. It shows how a private, on-chain intelligence network becomes feasible: not just by promising privacy, but by architecting for it. The memory is yours. The compute is verifiable. The storage is encrypted, user-keyed, and under your control.

Build today.
Visit Loyal

[website ](https://askloyal.com/)

and

[docs](https://docs.askloyal.com/)

.
Sign up for open testing in the

[Loyal community](https://tr.ee/4PzO0_KgFk)

.
Buy

[$LOYAL](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)

ownership token.
Check

[Nillion docs](https://docs.nillion.com/build/private-storage/overview)

.
`,r=`---
title: "How To Control the Future (not vote on it)"
date: "2025-11-28"
hero: hero.jpg
description: "Most DAOs are broken because they use \\"voting\\", but voting is free, which means people can lie, larp, or vote for stupid ideas without consequences."
author:
  name: "Rodion, founder"
---
Most DAOs are broken because they use "voting", but voting is free, which means people can lie, larp, or vote for stupid ideas without consequences.

Loyal is different - we use

[@MetaDAOProject](https://x.com/MetaDAOProject)

for governance which means we don't ask you to vote; we ask you to trade in your own best interest.

This system is called "Futarchy." It sounds complex, but it’s actually very simple. It’s based on one rule: Money talks, bullshit walks.

## The Concept: Two Timelines

Imagine you have a time machine that lets you see two different futures:

- Timeline A (PASS): We execute the Buyback plan.

- Timeline B (FAIL): We do not execute the Buyback plan.

In MetaDAO, we create a temporary token for each of these timelines.

1. [$LOYAL-PASS](https://x.com/search?q=%24LOYAL-PASS&src=cashtag_click) (The token price if we say YES)

2. [$LOYAL-FAIL](https://x.com/search?q=%24LOYAL-FAIL&src=cashtag_click) (The token price if we say NO)

*(Note: The same is true of USDC-PASS, and USDC-FAIL.)*

## How to Make Your Voice Heard

You don't cast a ballot. You simply buy the token of the future you believe is more valuable.

If you support the Buyback: You can either buy the PASS token, or sell the FAIL token.

By buying it, you push the price up. You are betting that

[$LOYAL](https://x.com/search?q=%24LOYAL&src=cashtag_click)

will be worth more if we execute this plan.

If you hate the Buyback: You can buy the FAIL token, or sell the PASS token.

In this case you are betting that the plan is a bad idea and

[$LOYAL](https://x.com/search?q=%24LOYAL&src=cashtag_click)

is better off without it.

## How the Winner is Decided

The market runs for 3 days (the Loyal buyback proposal will end just after 21:00 CET on Saturday, November 29th) so we'll be watching the price of both tokens.

- If the PASS token trades at a higher average price than the FAIL token, the proposal executes.

- If the FAIL token is higher, the proposal is rejected.

We don't care how many people like the idea. We care which idea makes the network more valuable.

## The Risk (Your Skin in the Game)

Here is why this system works better than voting: You have to be right.

- Scenario 1: You buy the PASS token. The proposal passes. Your PASS tokens convert into real [$LOYAL](https://x.com/search?q=%24LOYAL&src=cashtag_click) tokens. You kept your position and (hopefully) the price went up because it was a good idea.

- Scenario 2: You buy the PASS token. The proposal FAILS. Your PASS tokens become worthless. You essentially made a bad trade and lost on that specific position.

This forces you to think. You can't just blindly support everything. If you support a bad idea that tanks the price, you lose money. If you support a good idea that raises the value, you profit.

## Step-by-Step Instructions to make your voice count

![MetaDAO trading screen for the "Buyback Loyal Up To NAV" proposal: Pass at $0.2301, Fail at $0.2216, 50% likelihood](img-1.jpg)

The Loyal buyback proposal

1. Go to the MetaDAO Proposal Page. ([https://metadao.fi/projects/loyal/proposal/2VjKHNQdkLfHtoH1GtPVseJv1kP3VUoLGcZLc29SttgS](https://metadao.fi/projects/loyal/proposal/2VjKHNQdkLfHtoH1GtPVseJv1kP3VUoLGcZLc29SttgS)).

2. Connect your Solana Wallet.

3. Look at the Markets. You will see a "Pass Market" and a "Fail Market."

4. Make your move.

- Want the buyback? Buy [$LOYAL-PASS](https://x.com/search?q=%24LOYAL-PASS&src=cashtag_click).

- Don't want the buyback? Buy [$LOYAL-FAIL](https://x.com/search?q=%24LOYAL-FAIL&src=cashtag_click).

5.    Wait.

- After 3 days, if your side won, you claim your real tokens.

- If your side lost (e.g., you bought "Pass" but the proposal "Failed"), your prediction was wrong, and those specific conditional tokens go to zero.

## TL;DR:

Stop voting. Start trading.

If you think the Buyback makes Loyal stronger, buy the PASS token.

If you think it makes Loyal weaker, buy the FAIL token.

![Cover graphic reading "BUYBACK PROPOSAL FAQ" over a blindfolded statue of Justice pointing at a dystopian skyline](img-2.jpg)

## FAQ: User questions from the last 24h

Q: Why allocate $1.5M? This seems high?

*A: It sets a soft price floor.*

*We only buy if the price drops below the actual cash value of the treasury. The size is necessary to prevent arbitrage and signal that the DAO will defend its book value against vultures.*

Q: What if the defense fund gets fully spent?

*A: It changes nothing for the roadmap.*

*Our initial raise goal was $500k - we raised significantly more, so even if the full buyback executes, we have ample runway to deliver the stack and grow the network.*

Q: What happens to the money if it isn't used?

*A: The unspent stablecoins sit in the account until a follow-up proposal returns them to the main treasury. If the price respects the math, we spend nothing.*

Q: Is this designed to pump the token price?

*A: No. This creates a floor, not a ceiling.*

*The goal is to stop short-term flippers from exploiting the spread between the price and our cash value.*

Q: Why spread the orders over 30 days?

*A: To prevent volatility. We are using a recurring order (TWAP) to build a stable defense - we aren't trying to spike the chart artificially; we're simply helping the market respect the math.*

Q: Is this replacing the product launch push?

*A: No! This is purely financial hygiene for the DAO treasury.*

*The product ships when the code is attested and ready, and we don't time technology releases based on governance votes.*

Q: What is the buyback cooldown?

*A: Stability.*

*If this proposal passes, the DAO cannot entertain new buyback or redemption proposals for 3 months, which prevents governance spam and keeps the focus on building.*

Q: What's the TL;DR here?

*A: We are placing a soft price floor at 0.238 USDC*

*If the market prices*

[*$LOYAL*](https://x.com/search?q=%24LOYAL&src=cashtag_click)

*below the cash we have in the bank, the DAO buys it back*

*If the price respects the math, we spend nothing - if it doesn't, the DAO absorbs the supply and gets stronger*

Q: How do I vote?

*A: Loyal uses*

[*@MetaDAOProject*](https://x.com/MetaDAOProject)

*(a ‘futarchy’ model) for governance, you can find the proposal linked below:*

***Decide Loyal's most likely path to a higher market cap here:***

[https://metadao.fi/projects/loyal/proposal/2VjKHNQdkLfHtoH1GtPVseJv1kP3VUoLGcZLc29SttgS](https://metadao.fi/projects/loyal/proposal/2VjKHNQdkLfHtoH1GtPVseJv1kP3VUoLGcZLc29SttgS)
`,i=`---
title: "july: no kings or gods, just dogs"
date: "2026-08-11"
hero: hero.png
description: "We wanted to reach $300,000 in July. We doubled that and reached $600,000."
author:
  name: "Chris, founder"
  avatar: /blog/authors/chris.png
---
- We wanted to reach $300,000 in July. We doubled that and reached $600,000.
- 9,000 Seeker owners installed Loyal. Over 3,000 users launched an Earn agent.
- We passed the proposal and deployed the first Loyal autonomous vault for our MetaDAO treasury.

We launched the core Earn product the last week of June. Throughout July, growth was our only focus. Our goal for August is to keep growing active deposits and start securing the first business integrations.

In this article, we'll go over the growth of our deposits, highlight the product and governance updates that you might've missed!

![Title card reading "Meet Milo" over a red chevron pattern, with the Loyal mascot's eye and zig-zag grin above it](img-1.jpg "Can you guess what Milo has planned for August? (hint: it will make your dollars bigger)")

[Watch the clip on X](https://x.com/loyal_hq/status/2084130312884531473)

## We Doubled Our July Goal

We hit $390,000 halfway through July. We finished the month above $600,000.

That's nearly three times the $200,000 we reported in June and twice the baseline target we set for July.

The growth came after Seeker Summer put Loyal in front of thousands of new users. Around 9,000 Seeker owners installed the app, and more than 3,000 went beyond opening it and launched an Earn agent.

The important number is not installs. It is deposits.

People connected wallets, approved policies for their smart accounts, and trusted Loyal automations to manage real money. The same product that crossed $200,000 in June crossed $600,000 before July was over.

You can track the numbers at [stats.askloyal.com](https://stats.askloyal.com/).

![Loyal stats panel labelled "Earn AUM" showing $696,655.31, with the note "Current normalized Earn AUM" beneath it](img-2.jpg)

> Loyal hit 700k in TVL since our earn launch a month ago.
>
> We're enabling monetization soon after we finish some routing optimizations with all the revenue routed to the treasury.
>
> Also, closing our first B2B clients which should push the TVL to 7 figures.
>
> — [Rodion (@space_symmetry), Aug 3](https://x.com/space_symmetry/status/2084327765638307912)

## Updated webapp

In July, we shipped the rebuilt Loyal web app and improved the mobile experience. Users got clearer transaction details, historical earnings charts, QR receive, and an approval screen that explains each Earn transaction before it is signed.

We also improved the financial lifecycle behind the interface. A full withdrawal now closes the entire Earn setup in one flow and returns the remaining account rent. Realtime updates keep balances and transaction history current. Production monitoring records the cause of every failed flow so we can find problems quickly.

Loyal remains open source and onchain. Users keep custody of their money. Their smart accounts can only sign transactions that satisfy the permissions they approved.

This is the model we are building around: useful financial automation without giving up control.

![Red card reading "Meet the new web app" with the Loyal mascot's eye above the text and askloyal.com underneath](img-3.png)

![Overlapping Loyal web app cards: an Earn panel projecting $6,000 into $6,593 in a year at 9.89% APY, an APY chart putting Loyal Earn at 9.89% against Main Market at 5.66% and T-Bill at 4.22%, a total balance of $31,106.46 split across Earn, stablecoins and crypto, and a partly hidden $LOYAL token card](img-4.png "Welcome to the future of agentic finance. New webapp is live.")

## Governance Updates

LOYAL-003 passed in July and established the Loyal Autonomous Treasury Vault.

The vault is a Loyal Smart Account controlled by the DAO’s Squads treasury multisig and constrained by onchain policies. It can move capital between venues approved by the DAO and return funds to the treasury.

It cannot send funds to an outside address. It cannot sell LOYAL on the market, mint or burn tokens, or rewrite its own policy. Anything outside those limits still requires governance approval.

This gives the treasury room to automate routine operations without creating a new proposal and waiting through a multi-day governance market every time capital needs to move.

The vault went live on July 30. We rehearsed the flow with ten-cent deposits and five-cent withdrawals before moving real capital.

Its first authorized action withdrew exactly 50% of the DAO’s Futarchy AMM position: 176,688 USDC and 1,414,929 LOYAL.

Half of the USDC went into Loyal Earn. The other half returned to liquidity. The rollout had zero failed transactions.

We plan to sell policy-controlled treasury automation to other DAOs and businesses. Using it for our own treasury gives us a live proof point before we ask anyone else to trust it.

![Red card reading "New proposal is live" with the Loyal logo and askloyal.com](img-5.jpg)

[Read the proposal thread on X](https://x.com/loyal_hq/status/2080122227111137289)

## Q2 Report

We published the Q2 transparency report. Operating expenses from April through June were 172,839.34 USDC, and every line was disclosed and reconciled against onchain movements.

Q2 revenue was zero. Earn only launched near the end of the quarter. July gave us the first meaningful base of active deposits from which product revenue can grow.

We will keep publishing the numbers when they look good and when they do not.

![White card reading "Q2 2026 transparency report" with the Loyal logo and askloyal.com](img-6.png)

[Read the report announcement on X](https://x.com/loyal_hq/status/2079737101034385642)

## What comes next

July ended with more than $600,000 deposited, thousands of users, a stronger product, and a live treasury use case.

Next we are expanding Earn beyond USDC, building cross-mint routing, moving Watchdog forward, and turning the treasury infrastructure into business integrations.

The July goal was $300,000.

We cleared $600,000.

Now we keep going.

Always loyal,  
Chris

---

Website — [https://askloyal.com](https://askloyal.com)  
Docs — [https://docs.askloyal.com](https://docs.askloyal.com)  
Buy [$LOYAL](https://x.com/search?q=%24LOYAL&src=cashtag_click) on Jupiter — [https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)  
Chrome extension — [https://chromewebstore.google.com/detail/cdienfadefhlaknmedckgifkjdbioack](https://chromewebstore.google.com/detail/cdienfadefhlaknmedckgifkjdbioack)  
Telegram Agent — [https://t.me/askloyal_tgbot](https://t.me/askloyal_tgbot)  
Telegram Community — [https://t.me/loyal_tgchat](https://t.me/loyal_tgchat)  
Discord — [https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)  
X (Twitter) — [https://x.com/loyal_hq](https://x.com/loyal_hq)  
GitHub — [https://github.com/loyal-labs](https://github.com/loyal-labs)
`,a=`---
title: "Loyal for Seeker - But Why?"
date: "2026-04-24"
hero: hero.png
description: "Who we're building Loyal for and on our vision for decentralized, private agentic finance with human in the loop."
author:
  name: "Rodion, founder"
---
## TL;DR

Financial agents need wallet access to function, but handing them your private keys without guardrails is a disaster waiting to happen. Loyal on Seeker is the final form of decentralized, privacy-preserving, agent-powered finance.

## Rollout plan

→ **Phase 1**: Feature parity on the Solana Seeker. We are bringing private balances, handle-based payments, and shielded yield on USDC straight to your pocket.

→ **Phase 2**: Bounded automation via the introduction of smart accounts with on-chain guardrails a couple of weeks after our initial Seeker launch -  your agents will do the work of managing your private assets without accidentally buying Fartcoin while you're sleeping.

> [Learn more about Loyal](http://links.askloyal.com/)

## Details, details.

Agentic finance is here - if you're not onboarding, you'll be left behind - and your trading bots and portfolio rebalancers need wallet access to function. Right now, giving them that access means handing over your keys and hoping they do not hallucinate at 3 AM and drain your account into a memecoin.

Nobody solved the "what can be signed" problem. We did.

Loyal is the execution layer that makes agentic finance actually safe; you set the rules, the wallet enforces them on-chain, and the agent literally cannot exceed your limits.

We are proud to be bringing this entire stack to Solana's Seeker mobile phone, crypto is daily financial behavior -- it belongs on the worlds enthusiast crypto phone.

## Phase 1: Earn Yield on Private Assets

Before we can let agents run wild, we need the infrastructure in place, so our first step brings full feature parity from our web app directly to the Seeker dApp Store. This means you get a private, self-custodial account right out of the box.

- You get privacy a standard; shield and un-shield your Solana tokens at-will, send and receive without exposing your wallet addresses

- You get automatic APY on your shielded assets via [@kamino](https://x.com/kamino)'s single-asset lending vaults. You never have to choose between keeping your balance private and putting your capital to work.

- You get Telegram handle-based payments in addition to standard wallet addresses, so you can send SPL tokens to a Telegram @username instead of trading 44-character strings and hoping your clipboard survived.

Because we process the logic inside Confidential VMs, even we cannot see your data, the privacy we're talking about is baked into the hardware architecture as a baseline feature, not some afterthought.

![Three Loyal app phone mockups: "Send 1,600 USDC to @alex", a $7,226.88 balance card, and agent access spending limit](img-1.jpg)

Give us a try!

## Phase 2: Agentic Automation

This is the main event, and will be rolled out a couple of weeks after the Loyal Wallet hits the Seeker dApp store. Automation in crypto currently forces a terrible choice: sign every single transaction manually, or give a bot full custody.

Phase two removes the need for compromise.

We are bringing smart accounts with hard guardrails to Seeker.

1. You write the rule.

2. The account enforces it.

3. The software acts inside it.

Want a DCA agent that only buys specific tokens on a schedule? Define the token whitelist and set the spending cap. The agent will operate strictly within those parameters. Unverified contracts get rejected at the on-chain level.

You get bounded automation that executes while you sleep, while maintaining the our industry's founding ethos of decentralisation and privacy.

> Loyal Wallet on Seeker is where cypherpunk philosophy meets the hard-edge of consumer usability - your agent-powered, automated financial future; delivered without compromising your rights and freedoms.

## Why Seeker?

Firstly, mission parity - Seeker is the embodiment of functional, portable hardware security, and Loyal represents the same for agentic finance wrapped in cypherpunk ideals.

The Seeker dApp store also skips the bureaucratic red tape of Apple and Google, allowing us to ship faster and iterate with [@solana](https://x.com/solana) power users who actually push products to their limits.

... then there is the question of market size - the Seeker community is small, but while distribution matters, signal quality matters more.

We will be sponsoring gasless private transactions for every Seeker user to get things moving. We do not ask you to trust us with this rollout. We ask you to verify the hardware attestation. Trust the silicon, not the humans.

> [Learn more about Loyal](http://links.askloyal.com/) and prepare for the Seeker rollout.
`,o=`---
title: "Loyal Is Coming To Solana Seeker"
date: "2026-04-22"
hero: hero.png
description: "Great news! Loyal mobile app is coming to Solana Seeker in April!"
author:
  name: "Rodion, founder"
---

Great news! Loyal mobile app is coming to Solana Seeker in April!

We love the ecosystem that Solana Mobile team has fostered. Since August 2025, they’ve shipped over 200,000 devices and onboarded over 400 apps into dApp Store.

Loyal will be available for Seeker users through the Seeker dApp Store. We will be sponsoring gasless private transactions for every Seeker user!

## Why Seeker
We believe in importance of simplicity and distribution. Seeker has both.

There are over 200,000 Solana power users who have gone the extra mile in making Solana a core part of their daily lives. That’s distribution no one can compete with.

Unlike Apple’s App Store and Android’s Google Play Store, Seeker’s dApp Store has significantly less red tape we must tiptoe around to appease regulators. We can ship faster and start iterating with real users trying out a live app.

Securing our position among the Seeker userbase would give us a reliable platform to set up our further growth through iOS and Android.

![Loyal x Seeker promo: gasless private transfers, APY on Private Dollar, send over Telegram, with screenshot of USDC 5.65% APY](img-1.png)

## What Loyal Brings To Seeker
We are the only app that rewards you for keeping your money private. We are not charging swap fees or transaction fees intentionally. Our only incentive is delivering a great product and helping users multiply their money.

We believe that the best way to avoid catastrophic losses and multiple your fortunes is to keep it away from the prying eyes and make it easy to hold it long-term.

Hold your hard-earned cash and earn automatically!

## Android & iOS

Our current plan is to deliver Loyal app exclusively to seeker, iterate fast and capture devoted power users. We will focus on the full rollout on iOS and Android after.
`,s=`---
title: "Loyal Is Live On Solana Seeker"
date: "2026-04-28"
hero: hero.png
description: "Loyal is now available to all Solana Seeker users!"
author:
  name: "Rodion, founder"
---
Loyal is now available to all Solana Seeker users!

Today, Seeker users are getting access to the fastest privacy product on Solana.

Next week, we’re rolling out access to smart accounts and automated workflows for boring money tasks!

Download the app now to get a head start on what’s next.

Give us a try!

## Private Agentic Finance
We’re committed to building open-source products that push the frontier of financial autonomy on Solana.

The Seeker community is full of people who have committed to making Solana a daily part of their lives. These are the most excited users who want to see the ecosystem thrive. As we deliver brand-new ways of engaging with internet capital markets, we can’t imagine having a better beachhead than the Seeker community.

![Loyal quote card crediting Rodion, Loyal Founder: "Loyal on Seeker is the final form of agent-powered finance."](img-1.png)

We’re here to help users build and maintain healthy, long-term financial habits by making it easy to stay on top of your portfolio and earn money on private dollars.

For us, Loyal is about giving our users an easy and delightful way to multiply their wealth, while keeping the overall experience whimsical and heart-warming.

## Next Steps
We’re rolling out core features in two phases. Phase 1 brings our privacy features to Solana Seeker, Google Chrome extension, and our webapp. Phase 2 will empower our users with agentic automations to save time on boring financial tasks.

Starting today, everyone can earn up to 8% APY on each private dollar deposited in the Loyal app! All you have to do to start privately multiplying your wealth is make your first deposit.

If you’re anxious about missing out on today’s launch, don’t stress! We’ll be maintaining feature parity between our Seeker app, our Google Chrome extension, and our webapp. We’ll share more information on our iOS and Android launch after Phase 2 is complete.

Stay tuned for more updates coming soon, and thank you for being a part of our journey!

![Red graphic with white "DOWNLOAD NOW" text](img-2.png)
`,c=`---
title: "Markets Down. The Lower-Risk Play that Survives Red Days."
date: "2026-06-06"
hero: hero.jpg
description: "Bitcoin dropped below $60k and markets bled $2.5T. The lower-risk play that survives red days is boring: safe, bank-style yield on your stablecoins."
author:
  name: "Rodion, founder"
---
Bitcoin briefly dropped below $60k today, the US markets bled over $2.5t (**trillion, with a 'T'**), and while this is not unexpected to some seasoned traders - most investors can do better simply by being **boring** with their money.

The word "yield" has a **reputation problem** in crypto.

Say it to someone who has been around long enough and they picture **Luna**. Or an anonymous protocol promising 400% APY with no explanation of where the money comes from. Or the friend who lost everything in a "yield farm."

That reputation is earned because bad actors exist, and they destroyed the perception of a real business model that's been active in traditional finance for hundreds of years.

**But here is what most stablecoin holders don't know:**

The mechanism behind safe yield on crypto is identical to the mechanism your bank uses every day. The bank is doing it right now, with your money, without mentioning it.

The only difference is who keeps the margin.

## How Your Bank Makes Money on Your Savings

You deposit £5,000 in a savings account. The bank pays you 1.5%.

It lends the money to someone else - a mortgage holder, a small business, a car loan - at 5%, 6%, 7%. The bank keeps the difference. You get 1.5% because that's what it cost them to get you through the door. The rest is their operating margin.

Traditional banks take the majority of your yield

This is not a criticism of banks. It's just how lending works. Someone who needs capital pays someone who has it available. An intermediary facilitates the match and takes a [big] cut.

The mechanism has been working for centuries.

Here is the crypto version.

## The Same Thing, Different Pipes

Lending protocols like Kamino on Solana connect stablecoin holders with borrowers who need short-term liquidity - typically traders who want to open leveraged positions and need to borrow USDC or USDT to do it.

The borrower pays interest. That interest flows to the people who deposited into the vault. Kamino facilitates the match, manages the collateral requirements, and routes the funds.

**The mechanism is structurally identical to a bank savings account. Someone who needs to borrow pays someone who has spare capital. The intermediary takes a cut.**

The difference is the size of that cut.

A bank has branches. Staff. Marketing teams. Physical infrastructure. Shareholders. A DeFi lending protocol has smart contracts and audit reports. Its operating costs are a fraction of a traditional bank's. The margin it keeps is smaller. The rate it passes to depositors is higher.

Right now, Kamino's USDC vaults are paying between 4.5% and 5.5% APY. USDG vaults are at 9.5% to 9.8%. These are not promotional rates. This is what borrowers are currently paying to access liquidity on Solana - and it flows directly to depositors.

Compare that to what most people are actually earning:

- Euro area checking accounts: 0% to 0.05%

- US checking accounts: 0.05%

- Standard savings in the Netherlands: 0.25% to 0.50%

- Basic savings accounts: 1.0% to 2.0%

That last gap is not explained by risk. It is explained by habit.

Research shows that 82% of Americans don't use high-yield savings accounts despite the rates being widely available. In the UK, roughly 28% of people park most of their savings in a zero-interest current account out of routine.

Crypto is no different. Most stablecoin holders are earning nothing through habit or fear. The mechanism that would change that has been available for years.

## Why "Yield" Sounds Dangerous When It Usually Isn't

The association between crypto yield and disaster comes from a specific category of products and it's worth separating them cleanly:

**The things that went wrong:**

- Algorithmic stablecoins paying 20%+ with no auditable source of funds

- Protocol tokens minted to manufacture yield denominated in something that became worthless

- Cross-chain bridge exploits where collateral disappeared overnight

- "Yield farms" requiring simultaneous exposure to multiple volatile assets

**[@Kamino](https://x.com/Kamino)'s structural differences:**

- Single-asset lending vaults where you deposit one stablecoin and receive interest on it

- Over-collateralised loans where borrowers put up more collateral than they borrow

- Protocols with public audit histories, on-chain transparency, and significant TVL

![Loyal flow diagram: depositor lends stablecoins to yield provider Kamino, borrower pays interest, 6-8% APY earned](img-1.jpg)

Single-asset vault example on Kamino

*Note: all yield operations are done in the background for you when using the Loyal app, you simply approve, and your personal agent will ensure you always get the highest yield on your private, OR non-private funds.*

When you deposit USDC into a Kamino single-asset vault, you are doing one thing: lending it to borrowers who pay interest to use it.

- You can withdraw any time.

- You are not buying a token.

- You are not exposed to a volatile asset.

- You are not trusting a mechanism no one has reviewed.

The question worth asking about any yield is simple: **who is paying me, and why?**

On Kamino: a trader is paying you because they need your USDC to open a leveraged position. They put up collateral worth more than they borrow. If they don't repay, that collateral is liquidated to cover depositors. This is the same logic a bank uses to underwrite a secured loan.

A clean, verifiable source.

## The Part Nobody Says Out Loud

Your bank has been lending your money out for the entire time you have had a savings account. It has been collecting the interest spread, and it's been keeping most of it because banks are large, inefficient, and greedy.

You were not told because you did not need to be. You accepted whatever rate they offered and moved on.

DeFi lending does the same thing - except the protocol's operating costs are lower, so more of the rate flows back to you.

It is also worth noting the timing. The ECB meets on June 11, the Fed on June 17, the Bank of England on June 18. With inflation rising across most major economies, rate hikes are back on the table for the first time in years. If rates go up, traditional savings accounts may improve. But even if the ECB hikes back to 1.5%, Kamino's USDC vaults will still be paying 4% to 5%, and the outlier vaults will still be closer to 10%.

The infrastructure running this - Kamino's single-asset vaults - is the same infrastructure used by [@Phantom](https://x.com/Phantom), [@Anchorage](https://x.com/Anchorage), and [@pendle_fi](https://x.com/pendle_fi). Not exactly fringe protocols, right?

The only thing standing between a stablecoin holder and these incredible, low-risk returns is an over-complicated setup process where most interfaces assumed they already knew what they were doing.

**Loyal removes that assumption.**

You just deposit and approve; our upcoming yield optimizer will route your stablecoins continuously to the highest available safe rate across Kamino's vaults - meaning up to 11% APY for non-shielded assets. First yield credit within 24 hours, and you have the option to earn (less-optimized) yield on **private** stablecoins, a system unique to the Loyal app.

The mechanism was never exotic, the access just wasn't there.

***Now it is***

*-*

[*Learn more about Loyal*](https://links.askloyal.com)
`,l=`---
title: "May was good to us"
date: "2026-06-07"
hero: hero.png
description: "Seeker launch was a success."
author:
  name: "Rodion, founder"
---
Seeker launch was a success. It attracted over 70 reviews with an average rating of 4.2. Users created 18 new shielded pools that attracted over $15,000 in value in May. The most popular pools are USDC, followed by SOL, META and SKR. The launch got us a spot in the upcoming exclusive Seeker Summer event, which starts in July.

We shipped one-of-a-kind private yield. It takes $0.10 and one Solana block to spin up and gives you 8% APY on private dollars. Right now, it's *the* fastest, *the* cheapest, and *the* most financially responsible product for those who value privacy.

Within this newsletter, we'll go over those updates in more depth. We'll finish up with a note on our treasury, funds and runway. From now on, weekly videos will cover day-to-day things, while this newsletter will give you a clearer picture of the macro dynamics.

## Seeker goes loyal

Our Seeker app has shielded balances, swaps, sends and the same approval flow our users loved in our extension and web app. We layered push notifications, OTA updates for quick fixes and token due diligence pages to push that experience further. After the initial feedback, we also included raw instruction preview to make experience clearer for the users.

In a month since app launch, we've got 70 reviews with an average rating of 4.2. Most users are coming from the States, Japan follows after. Seeker users created 18 new shielded pools and brought $15,000 in value into them. The most popular pools are in the yield-bearing stablecoin pools, followed by SOL, META and SKR pools.

This traction and retention is the reason we prioritized Seeker. The launch was a success and landed us a spot in the upcoming Seeker Summer event in July. We'll be featured through Solana Mobile surfaces very soon. We'll leverage it to grow our metrics further.

## One Account, Every Surface

The web app went live in May, and it is the surface we have been moving toward all year. Open it, connect a wallet, and you land on the same shielded balance and yield logic that runs on mobile and desktop, in a browser, with nothing to install. That matters for the people who do not want another app on their phone or their dock.

What it ships with is the part worth pausing on. USDC shielding is wired straight to yield, so the moment your dollars are private they are also earning, up to 8% APY through the Kamino integration we have been refining for months. No second click, no separate vault. The point we have been making all year is now the way the app behaves by default.

The desktop app got rebuilt at the same time, from a sidebar to three panels. The old layout pinned tokens to one side and put the input in the middle. It worked, but it does not scale to what the app has to do now, which is hold a wallet, a multisig vault, and the wallets of any agents you have connected in one view. Three panels give us that, and the room to grow as automations take more of the screen. The new

[askloyal.com](https://askloyal.com)

landing went up to match, brighter and built around the product instead of slogans.

## A note on the treasury, OTC and runway

There's ~20K USDC in the treasury. We have ~100K USDC in operating wallets. We have 200K in hard commitments and more in soft commitments for the OTC deal in July. We have 400K in liquidity, we don't need as much after the buyback restructuring in January.

Our next step here is to raise a proposal that covers the OTC deal, liquidity rebalancing, migration to the new incentive program and other important updates. We work closely with gentlemen from

[@01Resolved](https://x.com/01Resolved)

to make sure you're well informed before the proposal goes live. Expect to hear more on that in mid-late June.

## What's next

Our first agentic use case goes live -- that's our Earn feature. We can push 9-11% yield on stablecoins without significant increases in your risk profile or having you give up custody of your finances.

After we're done with Earn on web app, we're giving you public dashboards to make it easy for you to see our numbers go up. We expect at least $300K in TVL by July.

If the engineering side scales the way we want it to, it'll make a lot of money to a lot of people. We hope you'll be among those people.

See you in the next one!

*Stay loyal,
Rodion*

Stay Loyal.

Website —

[https://askloyal.com](https://askloyal.com/)

Docs —

[https://docs.askloyal.com](https://docs.askloyal.com/)

Buy

[$LOYAL](https://x.com/search?q=%24LOYAL&src=cashtag_click)

on Jupiter —

[https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)

Telegram Agent —

[https://t.me/askloyal_tgbot](https://t.me/askloyal_tgbot)

Telegram Community —

[https://t.me/loyal_tgchat](https://t.me/loyal_tgchat)

Discord —

[https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)

X (Twitter) —

[https://x.com/loyal_hq](https://x.com/loyal_hq)

GitHub —

[https://github.com/loyal-labs](https://github.com/loyal-labs)
`,u=`---
title: "Loyal: Monthly Updates, Telegram Agent and GTM"
date: "2025-12-20"
hero: hero.png
description: "Breakpoint was a good experience."
author:
  name: "Rodion, founder"
---
## TL;DR

- Breakpoint helped us validate distribution + get feedback on what users actually want from Loyal Telegram Agent

- We’re shipping **Loyal Telegram Agent** first to win distribution + retention in crypto-native comms

- Next: ship summaries + storage + the first “real” agent loop **before the end of the year** (waitlist ~60)

## Solana Breakpoint

Breakpoint was a good experience. Only **2 people** from our team attended, which meant the rest of the team kept shipping without interruptions. We were able to distribute the app, get feedback, and pressure-test our assumptions in real conversations.

The most important conversations we had were:

- with **Squads** (with Stepan)

- with [**P2P.org**](https://p2p.org) and several large validators

The most useful feedback loop we got was around **Loyal Telegram Agent**. The takeaway is that people indeed want an AI tool that eliminates chaos in Telegram. There are plenty of tools for email workflows and almost nothing comparable for Telegram, even though Telegram is where most day-to-day communication in crypto actually happens.

We also talked to a lead from **Solana Seeker**, and we’re seriously considering launching on **Solana Mobile**. The user base isn’t huge, but it’s active, and distribution there is direct: ship something useful and it gets seen.

## Loyal Telegram Agent: things we shipped, and why we started here

Loyal is an infrastructure that allows developers to use AI for their on-chain apps. But if you want to build infra (especially on Solana), you still need user adoption and developer attention and the fastest way to earn both is to ship real apps. That’s why we’re releasing Loyal Telegram Agent first. It’s a clean wedge: distribution is natural, use-cases are obvious, and go-to-market is very easy.

We currently have ~50 people on the waitlist, and we’re using that group to validate what should ship first and what can wait.

## Why Telegram

Crypto is very different from the other industries in terms of how people communicate and just generally discover each other. Discovery in crypto happens on X, but the real communication doesn’t. It happens mostly in Telegram.

In other industries, people default to email/LinkedIn/WhatsApp. In crypto, founders, BD, operators, and anyone publicly visible ends up managing:

- Too many chats. I personally have to deal with around ~300 new messages every morning even though I read and clean everything before going to bed.

- Too many groups. The same goes fro groups: I constantly get distracted by new messages in chats which don't really require my attention.

- Spam is absolutely out of hand. I can be focused on work, get notification and open Telegram only to see that:

![Telegram spam DM from "Mr.Hamza" pitching Crypto Gems 1000X promo channels with t.me links](img-1.jpg)

![Telegram cold pitch from "Julia from P2B" offering CEX listing and marketing support across Europe](img-2.jpg)

And this is what every founder goes through.

Email had  more than 30 years of tooling evolution: clients, filters, smart inboxes, assistants. Telegram has almost none of that in a native-feeling way. Some Telegram CRM tools exist, but they’re separate web apps, they don’t feel native, and they  require more a lot of time to manage being useful only for sales/CSM peole.

That gap is what we're aiming to close with  Loyal Telegram Agent.

## What Loyal Telegram Agent is

1is a Telegram mini app with AI on top of Loyal infra. It can work with your personal chats, group chats, and new messages across Telegram. If you want, you can restrict access to certain chats using Telegram’s native tooling.

## What it does: shipped vs next vs later

Shipped (mainnet)

- **Solana transactions on mainnet** using Telegram handles

In progress (shipping before the end of the year)

- Chat summaries: summarize groups, highlight what matters, and help you catch up without scrolling

- User-owned storage + summary pipeline prepared

- Frontend and design for summaries are ready

- What remains: connect backend to frontend, connect summarization model, run initial tests

Later (roadmap)

- Chat prioritization: screen new messages and surface what actually requires attention

- Spam filtering: detect spam/scam patterns and route them out of your primary flow

- Payments + automations: “send 20 USDC to the most active community members this week” → sign → done, without collecting addresses

## Where GTM fits in

This is a strong go-to-market strategy for three reasons:

1. Clear target user that we can leverage for further distribution. We’re aiming at a specific pain point that founders and crypto BD live with daily: Telegram overload.

2. Retention is native**.**Telegram chaos doesn’t go away. If summaries and payments become part of the workflow, users return naturally.

3. Distribution is cheap**.**The app is currently free and we’ll have a free tier. The target audience is easy to identify, and we already talk to founder communities daily. Distribution cost here is close to zero.

## What this means for holders

Telegram agent itself isn't the end goal but rather a leverage.

If we get Loyal Telegram Agent into daily workflows for founders/BD/operators, we earn:

- repeated usage (retention)

- trust (especially around privacy expectations)

- distribution into teams and communities

- a direct channel to introduce the next layer of Loyal: developer tools and on-chain app integrations

That’s how this becomes meaningful at the ecosystem level: **a product that wins attention first**, then expands into infrastructure adoption.

## What’s next

- Finishing wiring summaries end-to-end (backend ↔ frontend ↔ model)

- Shipping the first full “summary loop” **before the end of the year**

- Onboarding from the **~60-person waitlist** and iterate quickly based on what they actually use

## Community updates

There are a lot of things we need your attention during the next week:

- Infographic competition, read more here: [https://x.com/loyal_hq/status/2002027637187772652](https://x.com/loyal_hq/status/2002027637187772652)

- Community members prepared proposal for withdrawing and burning 900k tokens from single-sided Meteora LP. We'll post more info on that in coming days.

- We're finishing the selection process for Sector 0 and applicants will know at the beginning of the next week if they were accepted. The next week will be packed so stay tuned.
`,d=`---
title: "Loyal: Open-source and E2E encryption won’t keep you safe"
date: "2025-10-16"
hero: hero.png
description: "Privacy, technology, and consumer choices have shifted."
author:
  name: "Rodion, founder"
---
Privacy, technology, and consumer choices have shifted. Application architecture has to keep up.

Web1 and Web2 apps that touched user data did relatively simple things. To send a private email, it was enough to encrypt the text with PGP. As the services developed, the architectural complexity grew with them. Soon, PGP was not enough and we looked for self-hosted servers for storage, messaging and collaborative docs.

Self-hosted solutions are a relic of the old paradigm. As a rule, even a microwave could run most of those services. That is not the case for modern LLM-driven apps. Realistically, very few people have enough GPUs to keep state-of-the-art LLM running locally – not to mention the electricity bill that would inevitably bring in. Even if a consumer (or a small business) is looking to host a smaller model locally, they’d have to spend hundreds – if not thousands – of dollars on hardware. LLM-centric services are bringing in the new paradigm and new challenges that we must respond to.

As we look at the modern privacy-focused solutions on the market, we must point out a glaring hole in their security model. Open-source SaaS solutions and E2E encryption (as implemented, for example, in Lumo) do not guarantee full privacy to the users. Even if we give hardware operators a benefit of doubt and assume that they’re actually running the code we see in repositories, they do not address the vulnerability of data as it moves between CPU and GPU kernels. GPU is as much an attack surface as anything else and hardware operators have full control over it.

![Diagram titled Encrypted Data Transfer in confidential GPUs, mapping host CPU, tenant VM and GPU in an 8-step secure flow](img-1.jpg)

The new era of artificial intelligence calls for the new approaches to security.

We argue that open-source products with hardware verification running within confidential VMs and confidential GPUs is the only viable solution that guarantees protection of user data. This setup ensures that even the hardware owners have no access to your data as it’s being processed. At the same time, we must utilize encryption and decentralized storage to allow everyone to move their chats across apps removing a single point of failure that is one vendor.

You can read more in the architecture docs:

[ docs.askloyal.com](https://docs.askloyal.com/)

.

## Open source and E2E is not enough anymore

Anyone and inspect open source code. E2E protects user data in transit and at rest. But neither of these things address fundamental concerns about data in use:

- LLMs are run on remote GPUs. The data appears in plaintext as it’s being processed.

- There’s no way for users to verify that the servers are running the same code they see on github.

- Root on the host, malicious hypervisor or a rogue admin (eg case of Facebook DMs) can have unprevented access to user data.

We cannot in good faith keep protecting the old paradigm. E2E and open source are necessary but they’re not enough to keep you safe in these times.

![Tweet from @jason warning of blackmail over leaked ChatGPT sessions, quoting Sam Altman @sama on ChatGPT restrictions](img-2.jpg)

## How Loyal works

Fundamentally, Loyal rebuilds the root of trust starting with the hardware. We use Intel chips with built-in hardware encryption keys. These chips allow us to do two important things: run virtual machines with encrypted RAM and take a cast of the launch parameters of each machine to show the code it is running. On top of that, we use Nvidia Hopper GPUs. They allow us to run confidential compute within them and guarantee that no one can access your data at no point in time.

## Request flow

1. **Client key generation**The client creates or loads an asymmetric keypair. The public key identifies the user. The private key never leaves the client.

2. **Remote attestation**Before any data moves, the client verifies an attestation quote from the target Loyal machine that binds the running measurement to the published Loyal binaries. If it matches, the client moves to the next step.

3. **Model execution inside the Loyal machine**The confidential service receives user inputs within the encrypted enclave. It passes data to GPUs running in confidential compute mode through a secure channel built upon mutual hardware attestation. At no point memory or context are unsealed and visible to anyone.

4. **Encrypted artifact storage on-chain**Response is then streamed back to the user and uploaded to decentralized storage attached to the user's public key. Only the user has access to it and can decrypt the data.

5. **Micropayments without API keys**The client pays for inference through onchain contracts tied to the session. There are no static API keys to manage, access is completely permissionless within network rules.

6. **Interoperability**The responses are streamed back and stored in OpenAI-compatible format. This removes dependency on our frontend and allows users to self-host it themselves or fork it and make any changes they deem fit. They can switch UI without the headache of database migration.

This is intentionally high level – see

[ docs.askloyal.com](https://docs.askloyal.com/)

for the architectural details.

## Loyal is just better

- **Trust minimization** – You do not grant blanket trust to a hosting provider. You verify the code identity and confine plaintext to a hardware-enforced enclave.

- **Verifiability** – Remote attestation gives a concrete, checkable statement about what runs where. This is stronger than contractual promises or opaque privacy policies.

- **Portability** – An encrypted, on-chain history lets you move between frontends and integrate with other tools without re-uploading or exporting data silos.

- **Permissionless access** – Sub-cent micropayments replace brittle API key distribution. Anyone can build a client that speaks the protocol and pays for what it uses.

## Why it matters

## Individuals

- Bring-your-own frontend that talks to the same encrypted history. Switch from a terminal UI to a web UI without migrating chats.

- Private cross-app memory under your keys – reuse context across projects without uploading files to multiple vendors.

## Teams

- Shared spaces with per-member keys and auditable access. Collaborate while keeping prompts and outputs unreadable to the operator.

- Least-privilege integrations – grant a bot access to a subset of artifacts by sharing only the needed decryption keys.

## Enterprises

- Regulated workflows with verifiable compute boundaries. Prove that sensitive prompts and outputs never left attested TEEs.

- Vendor flexibility – run against different TEE providers while keeping the same encrypted history and payment flow.

Services positioned as “private AI assistants” – for example,

[Lumo.ai](https://lumo.ai)

– aim to improve privacy within a conventional SaaS approach. The hard limit remains the compute trust boundary: without confidential machines, the host can access data in use. Loyal’s design addresses that exact gap while preserving cloud-class performance and model choice.

## Conclusion

Open source and E2E encryption remain essential. However, In cloud LLM settings, they are no longer sufficient on their own because the provider sees data in use. Loyal moves the trust boundary into attested hardware, stores artifacts encrypted on-chain for portability, and replaces static API keys with micropayments. The result is a practical step up in privacy without giving up modern models or the convenience of cloud compute.

If this matches the problems you are trying to solve, explore the architecture details at

[ docs.askloyal.com](https://docs.askloyal.com/)

.
`,f=`---
title: "Big Passive Rewards, from Stables you Already Own"
date: "2026-06-30"
hero: hero.jpg
description: "At Loyal, we love making money - and we know you do too."
author:
  name: "Rodion, founder"
---
At Loyal, we love making money - and we know you do too.

... but it's really easy to let fear and laziness cost you real gains, think about it:

1. Your stablecoins land in your wallet. A paycheck, a payment, a transfer from an exchange; whatever it might be.
2. Those funds just sit there for an hour, a day, a week, or maybe even forever before you do anything with them.
3. While they sit there idle, they can easily be earning substantial rewards for you.

You probably think the fix is "get some yield I guess?"

Kinda... unfortunately that also assumes you have the time and ability to navigate DeFi UX hell for what might seem like pennies in return. If you want the kind of APY that makes a real impact; you need to be manually watching pairs like a hawk all day every day - you don't have time for that, no one does!

Stablecoins actually have the ability to net you double-digit APY% consistently; but only if you're willing to make it a full-time job, staring at the various pairs and protocols all day long to do it.

![Collage contrasting a cluttered DeFi interface full of Approve/Reject prompts and APY charts with the Loyal app showing a balance earning 11.22% APY](img-1.jpg "'what am i even looking at!?' vs 'oh. big red rewards button.'")

That's where Loyal comes in - agentic finance that finally does something useful.

We just give you what you want (earning more, with low risk) automatically, with zero interaction by you after 60 seconds of setup: the biggest, passive rewards on stablecoins that you already own in your [@solana](https://x.com/solana) wallet.

## Who Loyal is for

If you hold stablecoins and they're not earning, it's for you.

If you've been burned in the trenches and want something real instead of a 100x promise that turns into a 100x loss, it's for you.

If you're new to crypto, and want to just get started without complicated wallet setups and with fast, visible results, it's for you.

![Red title card reading "EARN ROUTING GOES LIVE" with the Loyal logo and askloyal.com](img-2.jpg)

[Watch the clip on X](https://x.com/loyal_hq/status/2069918828558266525)

You don't need to know what a vault is. You don't need to follow rates. You don't need to have "figured out DeFi." Most people holding crypto haven't, and that's not a personal failing.

**90% of stablecoins on-chain earn nothing.**

The tools that could fix that have been too complex, too slow, or too expensive for the average holder to bother with.

You might have a few thousand dollars sitting in a wallet you're "fine" with. You might be a power user who's tired of babysitting positions. Either way, the job is the same: make idle money work, without making it your second job.

## What it does for you now

Our latest feature is called **Loyal Earn**, and it's live right now.

You deposit USDC, and an agent puts it to work across Kamino's highest-yielding safe vaults. Kamino is the same lending infrastructure used by Phantom and Anchorage, so the rates are public and real, not a number we made up. The agent doesn't park your money in one spot and forget it. It moves your funds to the best available safe rate, in real time, so you're always in a strong position without lifting a finger.

![Diagram: a Loyal agent asks for the highest, safest APY and routes the wallet into USDC at 10.3% APY, ahead of USDG at 6.4% and USDT at 8.2%](img-3.jpg)

On non-shielded stablecoins, that's **currently 10% APY.**

You also get a choice most wallets don't give you:

- **Keep your money transparent**, and the optimizer runs at full strength.
- **Shield your money for full privacy**, and you earn up to 8% APY.

Why the difference? The optimizer can't route funds it can't see. Shielding hides your balance from everyone, including the part of Loyal that chases the best rate. So privacy costs you a few rewards. For most people the 11% is the win and privacy is a nice bonus. For a few people privacy is the whole point, and 8% still beats almost everything else they could get.

Then there's the part that closes the gap I started with.

## The thing nobody else fixes

It's called **auto-deposit**, and it's the quiet hero of the whole app.

You set one number. Say you want to keep $100 liquid for spending. Anything above that, the app sweeps into Loyal Earn automatically, the moment it arrives. No clicking. No remembering. No "I'll do it this weekend."

![Loyal autodeposit settings keeping $100 liquid and sweeping everything above it from the main account into Earn, which holds 1,026.11 USDC](img-4.png)

Picture your paycheck landing on a Friday.

Normally it sits in your wallet until you get around to it, a couple of days - maybe a week later, earning nothing the whole time. With auto-deposit, the extra goes straight to work and starts compounding your rewards the second it lands, earning up to 11% APY from minute one.

One paycheck, one week early, doesn't sound like much. But this happens every cycle, all year, for as long as you hold money.

**The money you lose isn't in the rate you pick. It's in the days your cash sits still between getting paid and getting started.**

That's the part manual setups can't fix, because they depend on you remembering. Auto-deposit doesn't depend on you at all. It removes the one step that human beings reliably skip.

And your money never leaves your control. Loyal is self-custodial. Your keys, your funds, in Loyal Earn or anywhere else in the app. You can pull it out whenever you want, no lock-up.

## How to earn with Loyal

![The askloyal.com home page reading "Make your money bigger", with Get extension and Open web app buttons](img-5.jpg "askloyal.com")

Three steps:

1. **Click on "Open Web App"** at [askloyal.com](https://askloyal.com/). Log in with your Loyal wallet, [@phantom](https://x.com/phantom), or [@MetaMask](https://x.com/MetaMask). You don't need a new wallet to start.
2. **Deposit your USDC** into Loyal Earn and approve the transaction. Your money starts earning right away.
3. **Turn on auto-deposit** and set your spending threshold. Now every dollar above that line goes to work on its own, forever.

That's the whole setup. After that, the app does the job and you go back to your life.

We cover the wallet creation fees for you, and remove all the painful DeFi annoyances.

## What's coming next

Right now Loyal Earn lives in the web app.

It's coming very soon to Solana Mobile, Android, and Telegram. The goal is simple: put the same automatic earning everywhere your money already moves, so the gap between "got paid" and "earning" disappears no matter which screen you're looking at.

The direction is the same one we started with. Take the work of money management off your plate, keep your funds in your own hands, and let the boring, steady compounding happen in the background while you think about literally anything else.

Your stablecoins were taking a week off every month. Now they don't have to.

Stay Loyal.
`,p=`---
title: "How to Regain Control of your (Digital) Soul"
date: "2026-02-06"
hero: hero.png
description: "Every prompt you send to OpenAI or Anthropic is logged, stored, and readable by their staff."
author:
  name: "Rodion, founder"
---
**You’re using a wiretap to do your work.**

Every prompt you send to OpenAI or Anthropic is logged, stored, and readable by their staff.

How would it feel if you didn’t need to trust when using an LLM?

Not Sam or his peers. Not a BigAICorp. Not the tiny text in the ToS — just the code.

You and I both use LLMs daily, and by now we take it for granted — meaning we are not going back to the pre-LLM world. However, this world we find ourselves in is **already far more dystopian** than it seems.

You are training the models that will automate you by providing data which is extracted without your consent or any accountability — while you give up your right to privacy, and, as the cherry on top, pay a subscription fee for the ‘privilege’.

I’m a co-founder of Loyal Private Intelligence, and I’ll examine the data-extraction pipeline from scrapers to brokers to large companies that love to yap about how they bring humanity forward — forgetting to mention the catch. Then, we’ll work together to regain control.

## 1\\. There is no consent.

The industry standard is simple: You get convenience, they get your mind.

The phrase “Data is the new oil” is a cliché, but with the advent of LLMs, I have started to accept its premise. Why? Because I’ve seen petabytes scraped, packaged, and sold to LLM companies with no regard for users at all. **When you type or upload anything, you share it by default. That’s now considered the “industry standard”.**

You may be surprised by just how many people really believe that by clicking “do not share my data”, using “secret mode”, or such prevent ChatGPT from collecting and storing their data. The same goes for using social platforms and ‘private’ chats — everything is collected either by the platform itself, or by third-party extractors (or by both).

OpenAI’s ToS allow them to hand over data to law enforcement. If you are a founder asking about legal loopholes, or a regular person asking about sensitive personal issues, that text is permanent evidence.

## 2\\. There is no accountability

Why is data extracted at this scale?

*   **Training models.** Data is one of the major bottlenecks, especially for some types, contexts, and languages. AI companies purchase data for tens of millions of $ monthly.
*   **Targeting.** You may have seen the first ad experiments by major LLM companies, and everything we know about contemporary consumerism points to this being the default monetization in the future.

In short, **your data is getting extracted & processed for free, while the LLM company benefits from it**, and then can sell you more of anything you don’t need. Imagine the ad economy, but on steroids, and you pay instead of getting paid.

Now, what could you do against this?

What if your data gets leaked (which it, statistically, will)?

_… I mean, aside from not using LLMs._

## 3\\. There is no privacy

However, it doesn’t stop at targeting. The data needs to be stored, and may be compromised in so many different ways. And if you think it’s stored in the most secure fashion possible, just think again about the centralized servers.

For me, the main punchline is that any of my private or work-related information can be seen directly by people operating the LLM as a product, and then go into training datasets.

Moreover, my data could be used against me in court (which is openly stated by LLM corp execs), and if LLM corps see my AI startup as a competitor… well.

This will be financial data, medical data, confidential discussions, and generally sensitive info. Just not private by default. And what can we do about it?

As I said at the very beginning, this does feel dystopian to the point where you’d want to just look away: you don’t own your data, it’s basically stolen from you daily, and then used with disregard for your interests.

**But the good news is that we still can regain control**, and in this case, it actually starts with you and me.

There are only two realistic ways of dismantling the extraction pipeline: self-hosting and verifiable privacy:

*   **Self-hosting** is what you may already be using as an “advanced user”, but it’s fair to say it’ll not become the default or even a popular choice for most, including the most affluent.
*   **Verifiable privacy** was not practically viable at scale even ten years ago_,_ but today it could become the default answer to the pipeline problem. With it, you don’t trust people. Instead, you verify and rely on hardware-enforced confidential computing like TEEs / confidential VMs.

This is what we’ve built at [@loyal\\_hq](https://x.com/loyal_hq): we combined the speed of the data center with the privacy of self-hosting.

_Share your setup for secure inference. And I invite you to learn more about ours in the upcoming articles._
`,m=`---
title: "Smart Accounts: guardrails for AI agents"
date: "2026-01-21"
hero: hero.png
description: "Delegate bounded authority to an agent with on-chain spending caps and allowlists."
author:
  name: "Chris, founder"
  avatar: /blog/authors/chris.png
---

Handing an AI agent a wallet shouldn't mean handing it your whole balance. Loyal's Smart Accounts let you delegate bounded authority — and the bounds are enforced on-chain, not by trust.

## What you can enforce

- **Spending caps** — a hard ceiling on how much an agent can move.
- **Token allowlists** — restrict which assets an agent can touch.
- **Approved-protocol allowlists** — limit which programs an agent can interact with.

Because the policy lives on the Smart Account, the limits hold even if the agent's own keys are compromised.

## Privacy and autonomy compose

Private transfers keep an agent's activity off the public chain, while the on-chain policy still enforces caps and allowlists. You don't pick one — you get both on the same wallet.
`,h=`---
title: "LoyalApps: Solana transactions via telegram handle are live"
date: "2025-12-10"
hero: hero.jpg
description: "Privacy tends to become a narrative for the users while their onchain transactions are transaparent and AI-interactions stored on the 3rd party servers."
author:
  name: "Rodion, founder"
---
## Privacy matters

Privacy tends to become a narrative for the users while their onchain transactions are transaparent and AI-interactions stored on the 3rd party servers.

Loyal focuses on building tools and infrastructure using  private permissionless AI oracles powered by the

[@magicblock](https://x.com/magicblock)

.
Purpose is to provide services that offer privacy as a default value.

Today we're launching public version of the Loyal mini-app for our telegram users.

[https://t.me/askloyal_tgbot](https://t.me/askloyal_tgbot)

Now it's possible to send the Solana using telegram handle in 2 clicks and our private AI helps keeping information in the chats structured and away from the spam.

## How it works

Users can send Solana directly to any other account in telegram by knowing its handle.
No long wallet addresses required.
👉**Disclaimer:*****current app version supports only Solana transfers. Any other tokens sent will be lost.***

![Loyal cover graphic with two phone mockups: "Send to @alicia" 15 SOL and a "Claim 15.00 SOL" received card](img-1.jpg)

Use tg username to send SOL via Loyal app

## How to send Solana to your friend

In order to start using this method you need:
1) Get to the Loyal mini-app

[https://t.me/askloyal_tgbot](https://t.me/askloyal_tgbot)

2) Choose Send option

![Loyal app wallet screen with the Send button highlighted by an orange box, beside Receive and Mobile only](img-2.png)

3) Enter telegram @ handle
4) Choose SOL amount to send
5) Confirm

## How to claim received Solana

Once you receive any SOL transaction  you can get a notification if your friend shares it with you.

![Telegram message from @askloyal_tgbot: "You received +0.0100 SOL" from OviMusk to nelogx0, with an Open button](img-3.png)

Push notification of received transaction

## How to claim SOL

:
1) Open telegram mini-app

[https://t.me/askloyal_tgbot](https://t.me/askloyal_tgbot)

2) Find received transaction on the main page

![Loyal app activity list with two received rows showing green "Claim 0.25 SOL" and "Claim 0.001 SOL" buttons](img-4.jpg)

FInd the received SOL transaction on the main page and claim

3) Claim the amount
👉*Claims are gassless in the application.*

## AI powered chat navigation and anti-spam filter

Loyal App focuses not only on becoming as a 1 click Solana transfers tool.
It allows to organize the flow of inbox and group chats with the private AI solution.

![Loyal app Chat Highlights screen with Groups 5, Direct 4, Spam 3 tabs over a list of group chats](img-5.png)

Group chat summaries filtered by private AI  Loyal oracle

This will help user to avoid missing important news and alpha from the group chats.

As well as to lower the amount of spam messages just by filtering them.
Such option provides higher security against potential scam links and offers.

You can stay in contact and updated while continuing using telegram inside the Loyal app.

## Loyal plans

Loyal focuses on building tools for the community across the web3.
Team is open for dialogue. Therefore, if privacy topic is meaningful you can connect with us.

- Part of the core team attends the [@SolanaConf](https://x.com/SolanaConf) - so it's an opportunity to reach out IRL during this event

- There are regular community Friday streams - awesome place to get real-time updates about the project status

- Our core contributors can share the privacy vision among the platforms and we appreciate that. New Loyalty programme will go live in December [REDACTED]

We're welcoming you to join our socials
X:

[@loyal_hq](https://x.com/loyal_hq)

| TG:

[https://t.me/loyal_tgchat](https://t.me/loyal_tgchat)

| Discord:

[https://discord.gg/h878D2yBJ7](https://discord.gg/h878D2yBJ7)

Stay Loyal.
`,g=`---
title: "There will be two types of people in the AI era"
date: "2026-02-06"
hero: hero.png
description: "If someone printed your LLM chat history today and read it out loud, how screwed would you be?"
author:
  name: "Rodion, founder"
---
If someone printed your LLM chat history today and read it out loud, how screwed would you be?

We’ve got people who triple-check mixer routes, freak out about address reuse and then casually dump their questions about tax optimisation, side hustles, health issues, affairs, and leverage plays straight into centralized AI apps. Crypto is fighting surveillance on the financial layer for years now, but when AI showed up we all just… started talking.

## Mass surveillance is not fear-mongering anymore.

*   In the last ten years, just Meta, Microsoft, and Google alone complied with nearly 9 million government data requests, over a third from US agencies.
*   In the copyright lawsuit against OpenAI and Microsoft, the Times pushed for access to \\_around 20 million ChatGPT logs to analyze how the model was used over time.
*   The EU is literally trying to scan everyone’s private messages. Under the_“Chat Control”_ proposal, EU lawmakers have pushed rules that would allow to scan private including e2e encrypted chats.
*   Leaks from ChatGPT builds and job listings show it is building an ad platform inside ChatGPT. When you mix extremely personal data with an engine optimized, you don’t need much imagination to see where this goes.

Put those together and the shape is clear: LLM chats are becoming the new search history but longer, more intimate, better indexed, and more monetizable.

If AI becomes the front-end for everything you do, whoever owns the logs owns **you**.

## There will be two types of people in the AI era

**People whose entire AI life sits on Big Tech servers**.

Every fear, mischief, stupid idea, every “hypothetically” will be stored in a SaaS database, linked to your email, card and ready for discovery/leaks/ads optimization.

**People whose AI life sits behind crypto-grade privacy and on-chain guarantees**.

Chat history and all interactions with LLMs are bound to your wallet. Nothing is stored on centralized servers where someone can access your data. Compute runs in sealed hardware which just processes and forgets. Payments are on-chain, not in a Stripe customer profile.

We’ve seen this already. **CEX vs self-custody:** many people (including me back in 2017) learned the hard way that “not your keys, not your coins” is not a meme. **KYC chain vs privacy rails:** some people are okay with every transaction feeding surveillance companies; others route value through tools that don’t default to snitching.

**The next frontier is obvious:** not your keys, not your prompts.

In 2030, saying “yeah I just used best free AI apps at the top of the App Store” is going to sound about like yeah “I kept all my crypto on FTX”

## What Loyal is, in one sentence

**Loyal is permissionless private AI oracles on Solana.**

In plain language: you (or your app) send a prompt from your wallet, you pay on-chain, a sealed compute box answers you and forgets everything. No account, no required card payments, full privacy.

Under the hood, there’s a lot of work with TEEs, attestations, confidential GPUs, and MagicBlock infra to make it happen. You can learn more about the architecture here [docs.askloyal.com](https://docs.askloyal.com/)
 but we’ll skip the tech details for this article.

## For users: the last place you can tell an AI the truth

If we want to build an ecosystem, the first apps have to be made by us too. We are already finishing a live client including a Telegram miniapp where you can have private AI conversations, summarize chats, and start automating workflows, all paid via Solana and anchored to your wallet instead of an email/password combo.

What we do may sound like a niche today but so did “people who want to hold their own keys” in 2013.

## For builders: permissionless private AI oracles

If you’re building on Solana, “private AI oracle” is not a metaphor, it’s a pretty literal thing.

Mental model: as a dev, you don’t create an account with an AI SaaS, you call a Solana program, pass it encrypted input, pay, and get back an answer.

**A few obvious patterns:**

*   Private risk/compliance for DeFi. Let users sign certain data client-side, send it to Loyal in encrypted form, and receive back a risk score or classification. Your protocol logic only ever sees the output + a guarantee it came from an attested oracle.
*   AI co-pilots for wallets and neobanks. You want an agent sitting on top of user flows: summarizing their activity, flagging weird behavior, automating bill pays or swaps — but you don’t want your own servers hoarding chat logs forever. Loyal becomes the compute backend that sees and forgets.
*   Enterprise-ish workflows. For B2B teams dealing with chaos, invoices, payroll, client data: automated payments, knowledge graphs for org data, etc.

The key point for: **you don’t ask anyone’s permission** to use this. No sales call, no enterprise contract, not even account creation. If your program can send SOL/USDC, you can tap the oracle.

**The outlier slot in the Solana stack**

Most businesses in crypto end up with one of the obvious defaults like:

*   assets under management
*   asset issuance
*   volume

On Solana today, there is **no obvious default** for: “where do I route AI workloads that must not leak?” That slot is completely open.

![Tweet from Clutch @clutchxbt saying ZEC leads demand for private capital and Loyal will lead demand for private AI](img-1.png)

We’re not pretending there won’t be competition:

*   Big tech will roll out “enterprise privacy” features but their business model is still ads, surveillance, and compliance with nation-state data grabs.
*   Other teams will pitch “TEE + AI” ideas, but most are still at whitepaper stage, or will have no PMF.

![Tweet from Marvin Tong @marvin_tong saying there's no PMF for decentralized AI, listing what people need beside agents](img-2.png)

Our bet is humble and very direct:

*   AI isn’t a one-cycle meta; it’s the new interface.
*   Surveillance pressure only goes one way: up.
*   There should be at least one serious, crypto-native answer to “where do my most sensitive prompts live?”

We’re trying to make Loyal that answer on Solana.

## Closing

AI is already here. Surveillance is already here. The only thing that isn’t locked in yet is who your AI is loyal to. What can you do?

*   If you’re a **user**: try having your next uncomfortable conversation with [askloyal.com](https://askloyal.com/)
     Try to break it, drop in our community, give us feedback.
*   If you’re a **builder**: when you add AI to your product, ask whether your users would rather have those prompts on a vendor’s servers or in a sealed oracle they can verify.
*   If you’re a **holder**: if you think “private AI oracles on Solana” deserves to be a permanent part of the stack, LOYAL is the ownership coin wired into that bet.

AI is going to be the layer we talk to. We just want at least one place in that layer that stays loyal to the person on the keyboard.
`,_=`---
title: "Week 10 at Loyal"
date: "2026-03-30"
hero: hero.png
description: "No slides this week so we raw-dogged the stream."
author:
  name: "Rodion, founder"
---
No slides this week so we raw-dogged the stream. We shipped even better than usual.

## The Giveaway

Last week we pushed private transfers to mainnet, this week we stress-tested them. A thousand dollars, split across a hundred users. Different wallets, different usernames, tricky edge cases. Rodion ran a script that sent private Telegram transactions to all of them. Nobody needed a wallet. Nobody needed to set anything up. The money just arrived.

## Two things came out of it.

First, our smart contract needs optimization around account creation. At around 150 to 200 payments we burned nearly $90 in gasless fees. That is too much. The fix will likely come from compressed PDAs via Light Protocol. MagicBlock is building native support for that on their rollup.

Second, we confirmed what we suspected. Telegram handles are the easiest way to move money at scale. Easier than wallet addresses, easier than anything else we have tried. You type a name, the money moves, the recipient figures it out from there.

## The Browser Extension

Vlad built a browser extension. It works on every Chromium browser — Chrome, Arc, Brave, Opera, Edge. Firefox gets its own version. Safari is out for now.

It opens as a sidebar. You can create wallets, import them, send, receive, swap, and shield. Everything the mini app does, the extension does. The balance-hiding eye button is there too, for when you are on a screen that is not just yours.

We are shipping passkey support tied to [askloyal.com](https://askloyal.com/)
 so it works across every surface. For the extension specifically, we are looking at a PIN code instead of a full password. Your funds sit behind Squads and smart account protections already. We do not need to make you type eight characters every time you open a sidebar.

The extension is on our GitHub right now if you want it early. Public release is next week.

## APY on Shielded Assets

One of the real concerns about privacy is that your money stops working when you hide it. It just sits there, dark and quiet, earning nothing.

We are fixing that. When you shield USDT, USDC, or SOL, your deposit enters the Loyal private net. But on mainnet, those tokens get locked into a Kamino vault. They earn yield the entire time they are shielded. When you unshield, you get your tokens back plus whatever they earned.

Your money stays private and it stays productive. We think this is the first step into privacy-based earning and we have not seen anyone else do it.

## SDKs and CLI for Traders

We shipped TypeScript and Rust SDKs this week. Next week we ship a CLI client built on the Rust SDK with policy management inherited from Squads Smart Account.

This is for professional traders. Because your transactions run through our private layer, there is no front-running. No MEV. No sandwich attacks. The CLI plugs into agents — OpenLaw, Hermes, Codex — and we believe agentic finance will carry most of the trading volume within five years. We are building for that now.

## Mobile

We already have an Android APK with full feature parity against the Telegram mini app. Mobile apps ship in roughly two weeks.

## Next Week

Browser extension goes public. CLI ships. We keep optimizing the smart contract for cheaper private transfers. Mobile keeps moving toward release.

We stream live every Friday. Follow us on X and join our Telegram community to catch the next one.

Stay Loyal.

Website — [https://askloyal.com](https://askloyal.com/)
  
Docs — [https://docs.askloyal.com](https://docs.askloyal.com/)
  
Buy $LOYAL on Jupiter —  
[https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)
  
Telegram Agent — [https://t.me/askloyal\\_tgbot](https://t.me/askloyal_tgbot)
  
Telegram Community — [https://t.me/loyal\\_tgchat](https://t.me/loyal_tgchat)
  
Discord — [https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)
  
X (Twitter) — [https://x.com/loyal\\_hq](https://x.com/loyal_hq)
  
GitHub — [https://github.com/loyal-labs](https://github.com/loyal-labs)
`,v=`---
title: "Week 14 at Loyal"
date: "2026-03-31"
hero: hero.png
description: "In some languages the @ symbol is called “the dog.” We did not know that when we decided every Smart Account should be represented by a dog."
author:
  name: "Rodion, founder"
---
In some languages the @ symbol is called “the dog.” We did not know that when we decided every Smart Account should be represented by a dog. But it works out, because now when you tag your wallet in the chat, you are literally calling the dog. Hey @Spotty, swap this. Hey @Biscuit, rebalance that. The joke wrote itself.

Here is what we shipped this week and what is hitting mainnet next week.

## APY on Shielded Assets

![Screenshot of the Loyal app Stash at $9,267.67 with USDC earning 5.41% APY and SOL at 4.19% APY, titled Shielded Assets APY](img-1.png)

Last week we shipped Shielded Assets to mainnet. Private transfers, live on Solana. This week we looked at the shielded balances sitting there and realized something obvious: idle money on-chain is wasted money. It is rare anywhere in DeFi to hold an asset that earns nothing. We did not want to be the exception.

So we are integrating Kamino. Spot APY on USDC, SOL, and USDT. It is on devnet right now, our developer is testing it. Mainnet goes live early next week. By Thursday or Friday you will be able to earn yield on your shielded assets directly from the frontend and the extension.

Privacy and yield. No reason you should have to pick one.

We also optimized the shielding and unshielding routes. Both now take a single transaction instead of multiple. Cheaper gas, faster execution. That matters more once your shielded assets are earning and you are moving in and out more frequently.

## The Browser Extension

![Screenshot of the Loyal browser extension Send screen sending 12 SOL to @alicia with the Private Send toggle on](img-2.png)

Vlad demo’d the extension live on stream and it is the real thing. Side panel view in Chrome. You authorize with your wallet, your balance syncs, and you are in.

The important part: we are a general wallet provider now. Vlad connected to Jupiter during the stream to prove it. Tap connect, pick Loyal, done. Any dApp on Solana that supports wallet adapters can connect to us. We are on the same list as MetaMask and everyone else. That was the goal. Not a sidecar app. A wallet.

The extension ships to the Chrome Web Store next week. A couple of polish items left, import/export key management in settings, vault connection improvements. Then it is public. Everyone can download it.

## Smart Accounts and the Dogs

![Screenshot of Loyal Smart Accounts on desktop with dog avatars Rocky, Spotty, Buddy, Sparky, agent access and spending limits](img-3.png)

Smart Accounts are in closed testing on desktop. They ship publicly next week. Here is the idea.

You get two modes of agentic interaction. First mode: you add an agent to your Smart Account and it can suggest transactions. You review, you approve, it executes. This is the research pattern. Tell Claude or Codex to dig into a token, build the transaction, pass it to your wallet. You keep the final say. It is literally impossible for the agent to send money somewhere you did not approve. That unlocks every CLI tool and AI agent for Web3 without sacrificing custody.

Second mode is more autonomous. You set policies and spending limits per agent. The agent can rebalance your portfolio, optimize positions, operate within the permissions you defined. You do not even have to be there.

Now the dogs. Every Smart Account gets a unique identity generated from the hash of the wallet address. The first numbers in the hash determine the background color, the dog’s color, the dog’s emotion, and its name. A red background, angry dog, blue coat, named Spotty. Another wallet gets a calm green dog named Biscuit. You always know which account is which at a glance. Visual memory plus a name you can call from any text interface.

Our designer Alexander made the system and it is one of the most fun things we have shipped. Friendly, goofy, and nobody else does it this way.

## What is Next

Next week the extension goes public on Chrome. The week after that, we start on the mobile app. The architecture is similar to the extension so the timeline is tight. Goal is to have the mobile app submitted to the Solana Seeker dApp store within two weeks.

Once extension and mobile are live, you talk to the same stash from anywhere. Desktop, browser, phone. Suggest transactions, approve from whatever device you are on, never lose access, never compromise security. That is the setup we have been building toward.

Stay Loyal.

Website — [https://askloyal.com](https://askloyal.com/)

Docs — [https://docs.askloyal.com](https://docs.askloyal.com/)

Buy $LOYAL on Jupiter — [https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)

Telegram Agent — [https://t.me/askloyal\\_tgbot](https://t.me/askloyal_tgbot)

Telegram Community — [https://t.me/loyal\\_tgchat](https://t.me/loyal_tgchat)

Discord — [https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)

X (Twitter) — [https://x.com/loyal\\_hq](https://x.com/loyal_hq)

GitHub — https://github.com/loyal-labs
`,y=`---
title: "Week 15 at Loyal"
date: "2026-04-07"
hero: hero.png
description: "The extension is in the Chrome Web Store."
author:
  name: "Rodion, founder"
---

The extension is in the Chrome Web Store. Kamino APY is days away from mainnet. We hardened the wallet security, hashed Telegram usernames, and judged a community dog competition live on stream. Big week.

## The Extension Is in the Chrome Web Store

It is live but the public announcement will go live next week because want to change a couple of things before.

What you are getting: a full wallet in a Chrome side panel. Shielded assets, private transactions, send to Telegram, connect to any Solana dApp. Everything the mini app does, the extension does.

This week Vlad rewrote how authentication works. The PIN is gone because it could be vulnarable in some cases. It is now a proper password. If you already set a PIN, there is a graceful migration — enter your old PIN once, set a new password, done.

The dock animates based on what is happening in the wallet. Conditional actions show up depending on what you are looking at — shielded assets give you unshield and send, unshielded assets give you shield, swap, and send. And the big one: when you approve a transaction, the extension decrypts and displays every instruction. You are not blindly signing anymore. You see exactly what is happening, and if you want, you can inspect the raw data.

We connected to Jupiter on stream. You can swap tokens through any Solana dApp using Loyal as your wallet adapter. It works like Phantom or Backpack, except your transactions can be private and your wallet has an AI agent behind it.

## APY on Shielded Assets Goes to Mainnet

We are finishing integration with Kamino. Mainnet should be live within the next few days.

We made a deliberate call here: USDC only at launch. We looked at the data across Kamino and other protocols — the overwhelming majority of shielded deposits are USDC anyway. Adding SOL and USDT yield will come, but we are not going to complicate the UX for edge cases on day one. When it ships, it will be available on desktop, extension, and the mini app.

Privacy and yield. No reason you should have to pick one.

## Hashing Telegram Usernames

Small but meaningful update. Before this week, when you sent to a Telegram handle, the username was stored in a way that was not directly traceable to your funds — but it was still stored in plaintext. Now we hash the username as well. Additional private redundancy.

This one came from the community. Someone suggested it, we agreed it made sense, and we shipped it. That is how it should work.

## Privacy Narrative Alliance

People in our group chat kept asking what our community partnerships actually mean. Fair question.

The idea is simple: privacy is a narrative in crypto, and narratives move faster when multiple projects push them together. We partnered with Ghostware and others to cross-market, share audiences, and co-author content around the privacy use case on Solana and globally. Same values, shared reach. Shoutout to Rodion for running this.

## The Dog Competition

We ran a community competition for custom Smart Account dog designs and judged them live on stream. Some of the cools designs were a shark-dog hybrid that transformed the standard head shape into a fin. A Chinese Communist Party dog that earned a “plus 20 credit score” from Chris. An Abraham Lincoln dog. A Loyal Barack Obama.

The winner was Abraham Lincoln. A hundred dollars, sent through our private transactions. The recipient claims it through the mini app wallet.

We might turn these into regular weekly competitions starting on Mondays.

## What Is Next

Kamino APY mainnet and chrome extension public announcement.

Stay Loyal.

Website — [https://askloyal.com](https://askloyal.com/)
  
Docs — [https://docs.askloyal.com](https://docs.askloyal.com/)
  
Buy $LOYAL on Jupiter — [https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)
  
Telegram Agent — [https://t.me/askloyal\\_tgbot](https://t.me/askloyal_tgbot)
  
Telegram Community — [https://t.me/loyal\\_tgchat](https://t.me/loyal_tgchat)
  
Discord — [https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)
  
X (Twitter) — [https://x.com/loyal\\_hq](https://x.com/loyal_hq)
  
GitHub — [https://github.com/loyal-labs](https://github.com/loyal-labs)
`,b=`---
title: "Week 16 at Loyal"
date: "2026-04-13"
hero: hero.png
description: "It was a good week."
author:
  name: "Rodion, founder"
---
It was a good week. The kind where the work is there in front of you and it is real and it is done. We got our B1 token transparency filing live with Blockworks, pushed yield on shielded USDC toward mainnet across our apps, finished the browser extension for release, and kept reshaping the web experience so the product explains itself faster.

## The B1 filing is live

![Loyal B1 ICO Token Transparency Filing card, powered by Blockworks, with the Loyal fox logo](img-1.png)

We are now live with our B1 token transparency filing through Blockworks. It took a few rounds, a few questions, and a few iterations, but it is done.

This matters because serious capital does not do diligence by vibes. People want a framework. They want a report. They want something they can point at without having to reverse-engineer a startup from tweets and Discord messages. That is what this filing gives us.

For Loyal, this is useful well beyond one announcement cycle. It helps with future exchange conversations, future fundraising, and the broader job of making an onchain company legible to institutions without turning it into something lifeless.

We are starting the B2 filing next. That one is more about smart contracts, advisors, and outside relationships. Different surface area. Same goal: make the structure clear.

## Yield on shielded USDC

![Screenshot of the Loyal app Stash at $9,267.67 showing USDC earning 5.41% APY and SOL at 4.19% APY](img-2.png)

We are finishing the shift to mainnet for shielded yield. The current setup is simple in the way good systems should be simple.

When you shield USDC with Loyal, the asset moves into the yield layer on mainnet while your private ownership lives on our private rollup. You still get the privacy model we are building toward, but now the dollars do not have to sit there doing nothing. They can keep working.

The yield target discussed on stream was roughly 4 to 8 percent. That range matters, but the bigger point is the direction: private assets should not become idle assets. Privacy should not mean opting out of utility.

This is one of the product ideas we care about most. Hide the user from the chain. Do not hide the capital from productive use.

## The browser extension is basically done

![Screenshot of the Loyal browser extension panel showing $4,967.94, a Shield button, and USDC, SOL, USDT tokens](img-3.png)

The browser extension is finished, polished, and lined up for release next week. It is already in the Chrome Web Store. Right now it is in the final internal testing phase before we open it up.

This is one of those releases that changes how the product feels. Loyal stops being a thing you visit and starts being a thing that is simply there, inside the browser, ready to connect wherever you are. Chris put it well on the stream: seeing your own extension in the browser list feels different.

The team demoed it across live sites and wallet flows. Connect, open, use it, move on. They also added a few small but important touches, like easier hover actions and direct token-price links out to Jupiter. Small interface details, but they reduce friction and make the product feel less fragile.

There was one moment of stream honesty too. Chris thought the extension might be generating a new key on reopen. Vlad immediately said it was not. That is the kind of detail we like seeing in public. Ship the thing, test it hard, catch the weirdness, verify it, move forward.

## The site is being rebuilt around conversion

We have also been reworking the landing experience. The main web app is moving toward its own subdomain, while the main site gets much better at doing the first job a product site has to do: explain what this thing is and why anyone should care.

The big design idea is straightforward. Remove clutter from the middle of the page. Put the core actions up front. Make it obvious where to go next, whether that is the web app, the extension, or mobile.

The new hero section will use animation to show real use cases instead of throwing abstract claims at visitors. Agent-guided finance is easier to understand when you can see the flow in a few seconds. Idle funds move into yield. Actions happen clearly. The value lands fast.

There is still some goofy Loyal energy in there too. Moving eyes. Cute loaders. A product that does not feel dead on arrival. We like serious infrastructure, but we do not think serious infrastructure has to look miserable.

## Next week

Next week the browser extension goes public. The new site keeps moving. And if one more release lands before the end of April, you will hear about it soon enough.

Stay Loyal.

Website: askloyal.com  
Docs: docs.askloyal.com  
Buy $LOYAL on Jupiter: jup.ag  
Telegram Agent: t.me/askloyal\\_tgbot  
Telegram Community: t.me/loyal\\_tgchat  
Discord: discord.com/invite/tAwXsXwTv6  
X (Twitter): x.com/loyal\\_hq  
GitHub: github.com/loyal-labs
`,x=`---
title: "Week 17 at Loyal"
date: "2026-04-24"
hero: hero.png
description: "Two things shipped this week."
author:
  name: "Rodion, founder"
---
Two things shipped this week. The Chrome extension went live on the store and we iterated on it twice before Friday. The Q1 2026 transparency report went up on the docs. Most of the rest of the week went into preparation for a release at the end of April that we are not allowed to talk about yet.

## The Browser Extension

We pushed the extension to the Chrome store on Monday and immediately started watching how real users touched it. Two updates went out on Wednesday and Thursday off the back of that feedback. The thing looks and feels noticeably better than it did at launch.

The logo is new and animated. The unlock screen now has a proper “forgot password” flow so resetting a wallet is not a dead end. Real-time market quotes pull straight into the token list, so you see prices at a glance instead of clicking through. Every token now has its own detail page with a 24-hour chart, five-minute candles, your balance, holder distribution, and a trust score. USDC scored 81.7. Remember that number.

The token page also became the launch point for actions. If you open USDC and tap shield, USDC is already loaded in the shield screen. Same for SOL. Small thing. Removes a step every time you use it.

A user in China left feedback about the onboarding background being too gray. He was right. The next build swaps it to white across the board. Kudos to him.

## What Is Next for the Extension

One more polish update next week on backgrounds and small details, and then the extension team rolls fully onto the end-of-April release. If you know, you know. If you do not, you will have to seek the truth. Stay on the quest.

## The Q1 2026 Transparency Report

We have been pushing hard on transparency over the last few weeks. Part of that push was partnering with Blockworks on the B1 Token Transparency Framework, which is the lens institutional investors use to evaluate token launches. The other part was publishing our own Q1 report.

The report is live on [docs.askloyal.com](https://docs.askloyal.com/)
 under the transparency tab. It covers what shipped in Q1 2026, a month-by-month breakdown of the spend, and a written explanation of each bucket and what the money actually bought. Everything is verifiable on-chain. Rodion runs treasury and wrote it, which is how it got done in a day without pulling anyone off the product.

One number worth pointing at: travel was zero. We do not do conferences. We sit in San Francisco and write code. That is the advantage.

There was also a line in the report that looked a lot like a leak. Or a clue. We are going with clue.

## Next Week

Next week is the end-of-April release. That is the only thing that matters. Chris will be working out of the Coliseum offices to coordinate with the team there, and we are lining up a few media pieces to land alongside it. One more extension polish update ships on the way. After that the focus shifts fully to the launch.

Stay Loyal.

Website — [https://askloyal.com](https://askloyal.com/)

Docs — [https://docs.askloyal.com](https://docs.askloyal.com/)

Buy $LOYAL on Jupiter — [https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)

Telegram Agent — [https://t.me/askloyal\\_tgbot](https://t.me/askloyal_tgbot)

Telegram Community — [https://t.me/loyal\\_tgchat](https://t.me/loyal_tgchat)

Discord — [https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)

X (Twitter) — [https://x.com/loyal\\_hq](https://x.com/loyal_hq)

GitHub — [https://github.com/loyal-labs](https://github.com/loyal-labs)
`,S=`---
title: "Week 18 at Loyal"
date: "2026-04-26"
hero: hero.jpg
description: "The Loyal app for Solana Seeker landed in the store, the first smart account automations went into testing, and the mascot finally has a name."
author:
  name: "Rodion, founder"
---
The Loyal app for Solana Seeker landed in the store, the first smart account automations went into testing, and the mascot finally has a name.

Tuesday is the official Seeker launch and the rest of the week was built around getting there.

## The Seeker App

The Loyal app is on Solana Seeker. The first build is already in the store, but you should wait for the launch version on Tuesday. The early one is dusty.

Most of what makes Loyal work on the extension and the mini app is now on Seeker. Shielded assets, swaps, sends, receives, the same approval flow. We added a few things specific to mobile. Backgrounds you can swap that conditionally restyle the top bar. Real-time balance and activity updates that propagate the moment a transaction lands. Push notifications when someone sends you money or any other action you care about hits the wallet. Over-the-air updates so we can ship a fix in five minutes without anyone redownloading anything. Vlad caught a small bug live on stream and patched it before the section was over.

Every token now has its own detail page: 24-hour chart, market data, holders, trust score, an about section pulled from CoinGecko. From the same page you can tap shield, swap, or send and the token is already loaded into the action. One less step every time.

The transaction approval screen got a serious upgrade. We now show every instruction in a transaction, every required permission, and the raw data if you want to inspect it. That came directly from users who told us the old version asked them to sign things they could not see. Fair complaint. Fixed.

The library is on Seeker too. Deep dives and FAQs with proper prev and next navigation, so you can read through a topic the way you would read a chapter.

## The Built-In Browser

Seeker ships with a full browser inside the app and a directory of 101 trusted Solana applications. You tap an app, connect Loyal, approve, and you are in. Jupiter works. The Loyal web app works. Anything you would expect to work, works.

## Smart Accounts and Agent Wallets

![Smart account automation mockup: $16,267.48 total with Stash, Spotty and Buddy agents and a prompt to earn 4.8% APY](img-1.jpg)

We started testing the first smart account automations this week. The first one is simple: if you have more than 100 USD un stablecoins sitting idle, the wallet will auto-shield them so they earn yield instead of doing nothing. That is the kind of behavior the wallet should have by default.

The bigger piece is the CLI integration we are finishing now. Once it ships, agents like Hermes and OpenClaw can be given a Loyal smart account with narrow authority over a specific set of actions. Your agent gets to act on your behalf without ever seeing your private key. That is structurally better than any custodial setup these agents currently use, which means it has a real shot at becoming the default. Smart accounts merge into the production app on Tuesday or Wednesday.

## Milo

The dog has a name. Milo. Calling it the dog or just Loyal was getting awkward. It is Milo now. The introduction goes out properly alongside the launch.

## The New Landing

The new landing site goes live next week. Same direction we previewed in week 16 — fewer slogans, more product, animated examples of what Loyal actually does. It pairs with the Seeker launch.

## Next Week

Tuesday, April 28, the Seeker app goes live. Two or three articles, three podcasts, and a launch video are lined up around it. Smart accounts ship to production midweek. Milo gets his proper introduction.

Stay Loyal.
`,C=`---
title: "Week 19 at Loyal"
date: "2026-05-03"
hero: hero.png
description: "The Seeker launch is behind us, the new landing site is live, and the three-panel desktop app is days from production."
author:
  name: "Rodion, founder"
---
The Seeker launch is behind us, the new landing site is live, and the three-panel desktop app is days from production. We also got the thing we actually wanted out of the launch: an invitation into Seeker season. A short list this week, but the items on it are the ones that matter.

### The Seeker Launch

![Seeker Launch graphic: phone showing the Loyal app store listing, "Autopilot for your money", 4.5 stars, Install button](img-1.png)

We launched on Solana Seeker. That alone is worth pausing on. Loyal in the official store, in the hands of Seeker holders, with the same shielded balances, swaps, sends, and approval flow we built across the other surfaces. The campaign around the launch was not the biggest Seeker has seen this year, but for the resources and connections we had going in, it landed.

The bigger payoff is what came next. The launch was strong enough to earn us a spot in Seeker season, which means more distribution, more eyes, and a longer runway with an audience that actually uses the wallet on a phone. The next two or three weeks decide how much ground we win out of that. We think it can be a lot.

### The New Landing

![New Landing graphic: red askloyal.com homepage, "Put your money on autopilot", $16,267.48 balance and approvals](img-2.png)

The new askloyal.com is live. It is bright, loud, and built to stay with you whether you are on it for two seconds or fifteen minutes.

The copy is not where we want it yet. We had a second pass written but did not get it in front of the test group in time, so it stayed on the bench for the week. The updated text ships next Friday with the rest of the polish. The shape and the feel were the parts we wanted to lock in first, and those landed.

### The Desktop Rebuild

![Desktop App graphic: three-panel Loyal layout with wallet list, $16,267.37 balance, agent detail, and approvals](img-3.png)

The desktop app is moving from a sidebar layout to three panels. The old version pinned tokens to one side and put the input in the middle. It worked, but it does not scale to what the app actually needs to do — orchestrate a wallet, a multisig vault, and the wallets of any agents you have connected, all in the same view.

Three panels give us that. It also opens the door to more fluid interfaces, which matters once smart accounts and automations take more of the surface. The alpha pushed close enough to the Friday cutoff that we are holding it rather than shipping something rough. One more weekend of testing, and it goes to production around Monday.

### Smart Accounts Next

Smart accounts roll out everywhere next week, and the marketing cycle around them starts Wednesday. The work between now and then is the same three things: smart accounts have to behave, the automations have to behave, and the app has to make sense the first time a Seeker holder opens it. If we get that right, we can ride this into Seeker season instead of recovering from it.

### Next Week

Smart accounts ship. The desktop rebuild ships. The landing copy gets its second pass. Marketing for smart accounts kicks off Wednesday. Seeker season begins.

Stay Loyal.

Website — [https://askloyal.com](https://askloyal.com/)
  
Docs — [https://docs.askloyal.com](https://docs.askloyal.com/)
  
Buy $LOYAL on Jupiter — [https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)
Telegram App — [https://t.me/askloyal\\_tgbot](https://t.me/askloyal_tgbot)
  
Telegram Community — [https://t.me/loyal\\_tgchat](https://t.me/loyal_tgchat)
  
Discord — [https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)
  
X (Twitter) — [https://x.com/loyal\\_hq](https://x.com/loyal_hq)
  
GitHub — https://github.com/loyal-labs
`,ee=`---
title: "Week 20 at Loyal"
date: "2026-05-11"
hero: hero.jpg
description: "Lower fees on shielded balances are live, Smart Account policies are in the build, and the mobile app crossed enough reviews to give us real signal."
author:
  name: "Rodion, founder"
---
Lower fees on shielded balances are live, smart account policies are in the build, and the mobile app crossed enough reviews to give us real signal heading into the Google Play release.

Smaller week on the surface, but the pieces lining up behind it are the ones we have been waiting to ship.

## Lower fees on shielded balances

We rewrote the smart contracts behind shielding and the fees came down with them. Same flow, same privacy, less to pay every time you move money in and out. This one matters more than the line item suggests. Shielding has to be cheap enough that you stop thinking about it, otherwise people only do it on the way in and never on the way out. Cheaper fees push us closer to that.

## We are using our own wallet

Two weeks ago the team crossed a line we have been walking toward all year. We are now using Loyal as our daily wallet. Not the test build, not a sandbox account, the same app we ship to everyone else. The reason it stuck is the same thing we have been telling people for months: there is no other app where you do not have to pick between privacy and yield. Once that became true for us, the switch made itself.

Eating your own cooking is the only honest way to find what is broken. Expect the next few weeks of fixes to be sharper because of it.

## Smart account policies

![Smart account policies graphic: rule deposits USDC above $100 to Kamino USDC Vault monthly on day 15](img-1.jpg)

The smart account policy layer is in build. The idea is the one we have been pointing at since smart accounts started: an on-chain guardrail you set once, that the wallet enforces forever. Spending limits, whitelisted contracts the agent is allowed to touch, and the rest of the controls you would want before you hand a key to anything that runs on its own.

The shape of the problem is what makes this worth doing. Today you either give an agent a private key and pray, or you stand up a separate policy server with something like Open Wallet and babysit it. Both are bad answers. Ours is a multisig with the rules written on chain, so the agent only spends what you let it spend, and there is no second server to keep alive. We showed an early UI on stream this week. It will change a few more times before it ships, but the pieces are real.

## Mobile, and the road to Google Play

The Seeker release has pulled in close to fifty reviews at a 4.2 average. That is the same rating Sanctum is sitting on, which is a useful checkpoint for an app that has been in real hands for a few weeks. More importantly, the reviews gave us a punch list. The next mobile cycle is polish driven by what people actually said, with a Google Play release behind it for the audience that does not have a Seeker.

## Next week

Smart accounts ship. Mobile gets the polish pass. Google Play prep continues. Fewer slides, more shipping.

Stay Loyal.
`,te=`---
title: "Week 21 at Loyal"
date: "2026-05-19"
hero: hero.png
description: "The new web app is live with automatic smart account creation, and multisig out of the box."
author:
  name: "Rodion, founder"
---
The new web app is live with automatic smart account creation, and multisig out of the box.

## The Web App

The web app is the surface we have been moving toward all year. Open it, connect a wallet, and you land on the same shielded balance and yield logic that runs on mobile and desktop, in a browser, with nothing to install.

What it ships with is the part worth pausing on. USDC shielding is wired straight to yield, so the moment your dollars are private they are also earning, up to 8% APY through the same Kamino integration we have been refining for months. There is no second click, no separate vault, no choice between privacy and yield. The point we have been making all year is now the default behavior of the app.

## Smart Accounts on Connect

The web app sponsors a Squads smart account for you the moment you connect the vault. No setup screen, no fees on your end, no walking a normie through what a multisig is before they can use the product. You connect, the smart account is there, and the policies we shipped last week are already wrapping it.

This closes the loop on smart accounts as a default. Until now, getting one meant going through a flow most people would bounce out of. Sponsored creation removes the cost and the friction in one move. The wallet you get on first connect is the one we actually want you to use.

## Multisig in the Browser

Multisig transactions are live in the web app. The Squads account behind your balance is a real multisig, and the web app speaks to it like one. That is what makes the sponsorship above worth doing, because a smart account without multisig is just a wallet with extra steps. Now it is a wallet with rules, signed the way it is supposed to be signed.

For the teams using Loyal as a shared treasury, this is the surface they have been waiting for. For everyone else it is the foundation the agentic features sit on top of, and the reason we can keep saying the agent only spends what you let it spend.

## Yield Routing Next

Yield optimization and yield routing ship next week. The short version is that the same shielded balance will move across the best available yield sources on its own, with the privacy and the policies still in place. We will say more when it is live. The long version is that this is the API we expect to be the strongest secure yield surface in the game, and the reason a lot of the plumbing this week looked the way it did.

## Next Week

Yield routing goes to production. The web app gets its first round of polish from real usage. More on the routing API when it lands.

Stay Loyal.

Website —

[https://askloyal.com](https://askloyal.com)

Docs —

[https://docs.askloyal.com](https://docs.askloyal.com)

Buy

[$LOYAL](https://x.com/search?q=%24LOYAL&src=cashtag_click)

on Jupiter —

[https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)

Telegram Agent —

[https://t.me/askloyal_tgbot](https://t.me/askloyal_tgbot)

Telegram Community —

[https://t.me/loyal_tgchat](https://t.me/loyal_tgchat)

Discord —

[https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)

X (Twitter) —

[https://x.com/loyal_hq](https://x.com/loyal_hq)

GitHub —

[https://github.com/loyal-labs](https://github.com/loyal-labs)
`,ne=`---
title: "Week 3 at Loyal"
date: "2026-01-17"
hero: hero.jpg
description: "Here's what we shipped this week and where we're headed."
author:
  name: "Rodion, founder"
---
Here's what we shipped this week and where we're headed.

## A Smarter Way to Send

Telegram transactions now work from desktop, no Telegram app required.

The new send flow understands what you're doing as you type. You enter an amount, it immediately highlights it. It automatically picks up the difference between wallet address and Telegram username.

Just type naturally and the interface adapts!

Once sent, recipients get a claimable link. You can also refund the transaction if you accidentally sent it to the wrong person.

## Introducing Canvas

This is the big one.

We've been experimenting with a new interface concept: a free-form 3D canvas where you drag, drop, and combine elements like a crafting system.

Your tokens are objects. Actions (send, swap) are tools. Drop SOL onto "Send via Telegram" and the transaction flow opens. Drag two actions together and they form a group — like apps on your phone.

The idea: your dashboard should be yours. A community manager might pin salary payments, sentiment analysis, and group highlights. A liquidity provider might want LP positions, price charts, and personal messages. Same product, completely different workspace.

We're also building Recipes. Those are saved action templates you can execute with one click. Set up "Send 0.1 SOL to

[@candyflipline](https://x.com/candyflipline)

once, reuse it forever.

Still experimental. But this is where we're going.

## Group Chat Summaries Are Live

Open the Loyal mini-app and you'll see summaries of every chat where the bot is installed.

Right now it summarizes every 24 hours. Soon you'll be able to customize the frequency. We'll be adding profile pictures, notifications when new summaries drop next week. The end goal is to score active contributors to the community and create automatic incentive payouts to top10 of them.

Summaries will also become draggable Canvas elements, so you can pin the communities you care about most.

![Screenshot of the Loyal app showing The Loyal Community group chat summary of 37 messages with feature cards](img-1.jpg)

Check it out yourself!

## Q4 2025 Report + Buyback Complete

We published our first transparency report covering October 21 – December 31. Every expense, every major event — all public. Find it in our website footer or pinned on X.

The buyback also concluded: **1.5M USDC** spent to acquire **~6.35M tokens**, now sitting in the treasury alongside existing holdings. These tokens can only move via DAO proposal and are earmarked for future fundraising (OTC deals with funds/investors).

With most sellers cleared out, upward movement requires fewer buyers. The market is lighter now.

## What's Next

- Continued iteration on Canvas

- Simplified send/receive + desktop refunds via SDK

- Anti-spam feature testing

- Upgrading our default model to GPT open-source (120B parameters) for better response quality

- Private inference directly in the Telegram bot — no need to visit the website

- Team expansion (more soon)
`,re=`---
title: "Week 5 at Loyal"
date: "2026-01-31"
hero: hero.png
description: "Infrastructure-heavy week!"
author:
  name: "Rodion, founder"
---
Infrastructure-heavy week! We migrated to a new database solution to speed up our feature velocity, introduced a brand-new light design concept to the mini app, laid the groundwork for private swaps and shielded assets.

## Community Chat Summaries — Now Live

Every 24 hours, our system generates a set of topics from everything discussed across your community chats. Those topics are served on the frontend for you to browse, and a brief digest message is posted directly in the group chat.

Sent daily and on command. An expanded summary is available in the mini app.

This is foundational work for branded agents, one of the paid features we see driving real revenue down the line.

The next iteration will let you deep dive into individual topics — seeing the main arguments, key quotes, charts, and conclusions from actual community discussions.

We want to think of it as a fundamentally new and easier way to stay on top of your communities.

![Screenshot of the Loyal app community summary for January 30, with topic cards on swaps and monetization across 346 messages](img-1.jpg)

Improved haptic feedback makes summaries feel genuinely physical to interact with

## Moving Past nilDB

This one was painful but necessary. The infrastructure we'd built on top of nilDB wasn't scaling the way we needed it to. The product was too raw and not feature-complete enough for production use. C'est la vie ¯\\_(ツ)_/¯

We moved everything to new secure databases running in confidential mode with SOC2 compliance.

It took serious effort — we had to rework a significant amount of internal logic — but we didn't see no other choice if we wanted to keep our pace.

This migration tightened up our feedback loop, it's dramatically faster now. Every part of the pipeline is now built on established, battle-tested infrastructure. From here, the job is scaling each part individually, which is a much more tractable problem.

Since we had to migrate off nilDB, we had to re-write our encryption module to meet the future decentralization expectations of the users.

The goal was twofold: make it work seamlessly with the new infrastructure, and leave the door open for decentralizing the service as we grow.

The full pipeline — capture, process, encrypt, store incoming messages — is now complete and production-ready. This unblocks a wave of features that were waiting on secure message handling.

## Light Design Concept

![Screenshot of the Loyal app light-theme wallet showing $533.67, 2.8475 SOL, Send/Receive/Scan, and a SOL activity feed](img-2.jpg)

The Loyal mascot blinks and twitches its ear. We're adding more expressions over time — the idea is that Loyal feels alive!

Vlad built a new light design concept in a single day, and honestly, it might be here to stay.

This isn't just a color-swapped dark theme. It's a different design direction entirely — minimalist, clean, and solid. No blur, no transparency. Every element feels intentional. In a sea of dark-mode-by-default crypto wallets (Phantom, Solflare, etc.), the light aesthetic stands out immediately.

## Swaps and Shielded Assets — What's Coming

We're building private ephemeral transfers over a private ephemeral rollup through MagicBlock. In plain terms: you'll be able to send assets to friends without publicizing the amount or the recipient.

Here's the roadmap:

- Swap support is expanding beyond native SOL to most available assets. The swap screen design is finalized and frontend implementation is underway.

- Regular swaps ship first. The design is ready, and token logic is being wired up on the backend now.

- Shielded swaps and transfers follow immediately after — built in close collaboration with the MagicBlock team.

Testing target: end of next week on Devnet. Mainnet target: end of the following week.

Going from Devnet implementation to Mainnet deployment within a week is aggressive. We're asking the community to help us test next week to make that timeline real.

## Plans for Next Week

- **Private message processing** — encryption pipeline is done, now wiring it into the product

- **Expanded summaries** — topic deep dives with main arguments and key quotes

- **Light design implementation** — rolling out across swaps and transfers

- **Desktop experience** — drag-and-drop swaps and consent flows

- **Telegram thread support** — the new Telegram bot threads feature lets you run multiple conversation threads with AskLoyal's TG bot, similar to how ChatGPT handles separate chats

- **Shielded asset testing** on Devnet — we'll need your help here

*We stream live every Friday. Follow us on*

[Twitter/X](https://x.com/loyal_hq)

*and join our*

[Telegram community](https://t.me/loyal_tgchat)

*to catch the next one.*
`,ie=`---
title: "Week 7 at Loyal"
date: "2026-02-14"
hero: hero.jpg
description: "It was a good week."
author:
  name: "Rodion, founder"
---
It was a good week. The kind where you sit down on Friday and the work is there in front of you and it is real and it is done.

Here is what we did.

---

We built a way to hold crypto so nobody can see it. You put coins in your Loyal wallet and they go dark. Nobody sees how much. Nobody sees what kind.

![Loyal wallet activity list headed "Shielded Assets", showing a shielded 8,000 USDC balance, an unshielded 0.2511 SOL entry, and swap, received and sent rows](img-1.jpg "The money is yours and the chain says nothing.")

Sends are invisible too! You pay someone and the money moves and it arrives and there is no line on a ledger for the world to read. No one knows you sent it nor who got it.

![Loyal send screen headed "Private Transactions", composing 465 USDC to @alex with a sent confirmation, sending over a Telegram username instead of an address](img-2.jpg "We made sends invisible too!")

The sends work on their own too. A bot can send. A script can send. An agent we build can send. The person on the other end opens Telegram and the money is waiting.

We built this because our agents will need to pay people. To run a community you need to pay the people who show up and do the work. Now that can happen without a person in the middle.

Both features are live on devnet today. We push them to mainnet next week.

## Summaries in Ten Groups

We expanded our testing community to 10 group chats. Rodion watches the numbers every day. Who opens the summaries. What time. Whether they come back the next day and the day after that.

You put a thing in front of people and you sit and watch and move one small piece and sit and watch again. There is no shortcut for this part.

But the thing underneath it is big. There are a lot of people who do this work in crypto. Community leads and BD people and DevRels and founders and marketing leads. They all live on Telegram. They wake up and there are hundreds of messages and they read for two hours before their real day starts. We want to hand them back that time. Two hours each morning for hundreds of thousands of people.

This is the slow work, but that is what we are after.

---

![Loyal app screen titled "Confirm You're Human" asking for a quick biometric check, with a Verify with Face ID button](img-3.jpg "Show you are real!")

Next week we ship something new that I have been thinking about for a long time.

We built a way to check that you are a real person. Your phone looks at your face or reads your fingerprint. The data stays on your phone. We never see it. We never store it. We just get one word back: yes or no.

We are putting it in DMs first. You know what your DMs look like. Half of them are not from real people. We are going to fix that. Then we bring it to group chats. Then to whole communities.

I think about the people who manage communities for a living. The ones who spend their mornings deleting spam before their actual work starts. This is for them.

---

We shipped onboarding screens for new users. People told us the app was confusing on first launch. They were right.

![Four Loyal onboarding screens: Group Summaries, Swipe Through Your DMs, Shielded Assets and Private Transactions](img-4.jpg)

## Next Week

We push private sends and quiet balances to the live network. We scale summaries past ten groups. We ship the human check to DMs.

See you next week.

*Stay Loyal.*
`,ae=`---
title: "Week 9 at Loyal"
date: "2026-03-24"
hero: hero.png
description: "The best take from the stream: there is a pretty famous streamer who bought an expensive watch with USDT while traveling in Nigeria."
author:
  name: "Rodion, founder"
---
The best take from the stream: there is a pretty famous streamer who bought an expensive watch with USDT while traveling in Nigeria. The shop owner could look up his wallet. Every person who ever bought anything from that shop, anyone who knew the account, could look up his wallet. They could see exactly how much money he had.

Transparency is great until it is not.

## This week we shipped private transfers on mainnet.

![Screenshot of the Loyal app Send screen sending 1,200 USDC to @alex with Private Send toggled on](img-1.png)

Most people do not know what a seed phrase is. They have never touched a private key. They have a Binance account and they have been using it for years and they are not changing. We know these people. We have tried to pay them the other way.

Now we send to their Telegram handle and it becomes their problem in the best way. The money arrives. They figure it out. Nobody gets a lecture about Solana.

And the sender stays clean. No wallet address shared, no history exposed. The chain says nothing.

The fees are 02%. Ten times cheaper than the major wallets that added private transfers recently. No minimum amount. Send half a SOL if you want — Solflare won’t let you, we will.

## The desktop is unrecognizable.

![Desktop Update graphic: askloyal.com desktop app with chat input, $1,267.47 balance, and a Shield swap panel](img-2.jpg)

Vlad has been building this for a while and this week it landed. Go to [askloyal.com](https://askloyal.com/)
 and you will feel the difference immediately. Full wallet integration. Everything the Telegram mini app does, the site does now. Private sends. Shield swaps. Portfolio view. Balance chart.

We added some eye-candy too: the dog follows your cursor when it is happy. It gets bored if you ignore it. It hides its eyes when you hide your balance. We are not going to say more than that. Go poke it. We are not responsible for what happens.

## Chat memory

![Chat History graphic: Loyal desktop app sidebar listing past conversations with a New Chat button](img-3.jpg)

Chat history is live now. Every wallet keeps its own conversation history. Connect a different wallet and you are in a different thread. The sidebar shows your wallet state while you chat so when the AI moves money, you see it, you approve it, you never leave the window.

The private LLM at [askloyal.com](https://askloyal.com/)
 now holds context reliably. We have been working on this for a while. Memory is stable now. and your history persists across sessions. The messages are stored in a secure database and encrypted with your wallet private key.

Next week we keep building. Stay loyal.

Website — [https://askloyal.com](https://askloyal.com/)
  
Docs — [https://docs.askloyal.com](https://docs.askloyal.com/)
  
Buy $LOYAL on Jupiter —  
[https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta](https://jup.ag/tokens/LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta)
  
Telegram Agent — [https://t.me/askloyal\\_tgbot](https://t.me/askloyal_tgbot)
  
Telegram Community — [https://t.me/loyal\\_tgchat](https://t.me/loyal_tgchat)
  
Discord — [https://discord.com/invite/tAwXsXwTv6](https://discord.com/invite/tAwXsXwTv6)
  
X (Twitter) — [https://x.com/loyal\\_hq](https://x.com/loyal_hq)
  
GitHub — [https://github.com/loyal-labs](https://github.com/loyal-labs)
`,oe=Object.create,se=Object.defineProperty,w=Object.getOwnPropertyDescriptor,ce=Object.getOwnPropertyNames,T=Object.getPrototypeOf,le=Object.prototype.hasOwnProperty,E=(e,t)=>()=>(t||(e((t={exports:{}}).exports,t),e=null),t.exports),ue=(e,t,n,r)=>{if(t&&typeof t==`object`||typeof t==`function`)for(var i=ce(t),a=0,o=i.length,s;a<o;a++)s=i[a],!le.call(e,s)&&s!==n&&se(e,s,{get:(e=>t[e]).bind(null,s),enumerable:!(r=w(t,s))||r.enumerable});return e},de=(e,t,n)=>(n=e==null?{}:oe(T(e)),ue(t||!e||!e.__esModule?se(n,`default`,{value:e,enumerable:!0}):n,e)),fe=E(((e,t)=>{function n(e){return e==null}function r(e){return typeof e==`object`&&!!e}function i(e){return Array.isArray(e)?e:n(e)?[]:[e]}function a(e,t){if(t){let n=Object.keys(t);for(let r=0,i=n.length;r<i;r+=1){let i=n[r];e[i]=t[i]}}return e}function o(e,t){let n=``;for(let r=0;r<t;r+=1)n+=e;return n}function s(e){return e===0&&1/e==-1/0}t.exports.isNothing=n,t.exports.isObject=r,t.exports.toArray=i,t.exports.repeat=o,t.exports.isNegativeZero=s,t.exports.extend=a})),pe=E(((e,t)=>{function n(e,t){let n=``,r=e.reason||`(unknown reason)`;return e.mark?(e.mark.name&&(n+=`in "`+e.mark.name+`" `),n+=`(`+(e.mark.line+1)+`:`+(e.mark.column+1)+`)`,!t&&e.mark.snippet&&(n+=`

`+e.mark.snippet),r+` `+n):r}function r(e,t){Error.call(this),this.name=`YAMLException`,this.reason=e,this.mark=t,this.message=n(this,!1),Error.captureStackTrace?Error.captureStackTrace(this,this.constructor):this.stack=Error().stack||``}r.prototype=Object.create(Error.prototype),r.prototype.constructor=r,r.prototype.toString=function(e){return this.name+`: `+n(this,e)},t.exports=r})),me=E(((e,t)=>{var n=fe();function r(e,t,n,r,i){let a=``,o=``,s=Math.floor(i/2)-1;return r-t>s&&(a=` ... `,t=r-s+a.length),n-r>s&&(o=` ...`,n=r+s-o.length),{str:a+e.slice(t,n).replace(/\t/g,`→`)+o,pos:r-t+a.length}}function i(e,t){return n.repeat(` `,t-e.length)+e}function a(e,t){if(t=Object.create(t||null),!e.buffer)return null;t.maxLength||(t.maxLength=79),typeof t.indent!=`number`&&(t.indent=1),typeof t.linesBefore!=`number`&&(t.linesBefore=3),typeof t.linesAfter!=`number`&&(t.linesAfter=2);let a=/\r?\n|\r|\0/g,o=[0],s=[],c,l=-1;for(;c=a.exec(e.buffer);)s.push(c.index),o.push(c.index+c[0].length),e.position<=c.index&&l<0&&(l=o.length-2);l<0&&(l=o.length-1);let u=``,d=Math.min(e.line+t.linesAfter,s.length).toString().length,f=t.maxLength-(t.indent+d+3);for(let a=1;a<=t.linesBefore&&!(l-a<0);a++){let c=r(e.buffer,o[l-a],s[l-a],e.position-(o[l]-o[l-a]),f);u=n.repeat(` `,t.indent)+i((e.line-a+1).toString(),d)+` | `+c.str+`
`+u}let p=r(e.buffer,o[l],s[l],e.position,f);u+=n.repeat(` `,t.indent)+i((e.line+1).toString(),d)+` | `+p.str+`
`,u+=n.repeat(`-`,t.indent+d+3+p.pos)+`^
`;for(let a=1;a<=t.linesAfter&&!(l+a>=s.length);a++){let c=r(e.buffer,o[l+a],s[l+a],e.position-(o[l]-o[l+a]),f);u+=n.repeat(` `,t.indent)+i((e.line+a+1).toString(),d)+` | `+c.str+`
`}return u.replace(/\n$/,``)}t.exports=a})),D=E(((e,t)=>{var n=pe(),r=[`kind`,`multi`,`resolve`,`construct`,`instanceOf`,`predicate`,`represent`,`representName`,`defaultStyle`,`styleAliases`],i=[`scalar`,`sequence`,`mapping`];function a(e){let t={};return e!==null&&Object.keys(e).forEach(function(n){e[n].forEach(function(e){t[String(e)]=n})}),t}function o(e,t){if(t||={},Object.keys(t).forEach(function(t){if(r.indexOf(t)===-1)throw new n(`Unknown option "`+t+`" is met in definition of "`+e+`" YAML type.`)}),this.options=t,this.tag=e,this.kind=t.kind||null,this.resolve=t.resolve||function(){return!0},this.construct=t.construct||function(e){return e},this.instanceOf=t.instanceOf||null,this.predicate=t.predicate||null,this.represent=t.represent||null,this.representName=t.representName||null,this.defaultStyle=t.defaultStyle||null,this.multi=t.multi||!1,this.styleAliases=a(t.styleAliases||null),i.indexOf(this.kind)===-1)throw new n(`Unknown kind "`+this.kind+`" is specified for "`+e+`" YAML type.`)}t.exports=o})),he=E(((e,t)=>{var n=pe(),r=D();function i(e,t){let n=[];return e[t].forEach(function(e){let t=n.length;n.forEach(function(n,r){n.tag===e.tag&&n.kind===e.kind&&n.multi===e.multi&&(t=r)}),n[t]=e}),n}function a(){let e={scalar:{},sequence:{},mapping:{},fallback:{},multi:{scalar:[],sequence:[],mapping:[],fallback:[]}};function t(t){t.multi?(e.multi[t.kind].push(t),e.multi.fallback.push(t)):e[t.kind][t.tag]=e.fallback[t.tag]=t}for(let e=0,n=arguments.length;e<n;e+=1)arguments[e].forEach(t);return e}function o(e){return this.extend(e)}o.prototype.extend=function(e){let t=[],s=[];if(e instanceof r)s.push(e);else if(Array.isArray(e))s=s.concat(e);else if(e&&(Array.isArray(e.implicit)||Array.isArray(e.explicit)))e.implicit&&(t=t.concat(e.implicit)),e.explicit&&(s=s.concat(e.explicit));else throw new n(`Schema.extend argument should be a Type, [ Type ], or a schema definition ({ implicit: [...], explicit: [...] })`);t.forEach(function(e){if(!(e instanceof r))throw new n(`Specified list of YAML types (or a single Type object) contains a non-Type object.`);if(e.loadKind&&e.loadKind!==`scalar`)throw new n(`There is a non-scalar type in the implicit list of a schema. Implicit resolving of such types is not supported.`);if(e.multi)throw new n(`There is a multi type in the implicit list of a schema. Multi tags can only be listed as explicit.`)}),s.forEach(function(e){if(!(e instanceof r))throw new n(`Specified list of YAML types (or a single Type object) contains a non-Type object.`)});let c=Object.create(o.prototype);return c.implicit=(this.implicit||[]).concat(t),c.explicit=(this.explicit||[]).concat(s),c.compiledImplicit=i(c,`implicit`),c.compiledExplicit=i(c,`explicit`),c.compiledTypeMap=a(c.compiledImplicit,c.compiledExplicit),c},t.exports=o})),ge=E(((e,t)=>{t.exports=new(D())(`tag:yaml.org,2002:str`,{kind:`scalar`,construct:function(e){return e===null?``:e}})})),O=E(((e,t)=>{t.exports=new(D())(`tag:yaml.org,2002:seq`,{kind:`sequence`,construct:function(e){return e===null?[]:e}})})),k=E(((e,t)=>{t.exports=new(D())(`tag:yaml.org,2002:map`,{kind:`mapping`,construct:function(e){return e===null?{}:e}})})),_e=E(((e,t)=>{t.exports=new(he())({explicit:[ge(),O(),k()]})})),A=E(((e,t)=>{var n=D();function r(e){if(e===null)return!0;let t=e.length;return t===1&&e===`~`||t===4&&(e===`null`||e===`Null`||e===`NULL`)}function i(){return null}function a(e){return e===null}t.exports=new n(`tag:yaml.org,2002:null`,{kind:`scalar`,resolve:r,construct:i,predicate:a,represent:{canonical:function(){return`~`},lowercase:function(){return`null`},uppercase:function(){return`NULL`},camelcase:function(){return`Null`},empty:function(){return``}},defaultStyle:`lowercase`})})),ve=E(((e,t)=>{var n=D();function r(e){if(e===null)return!1;let t=e.length;return t===4&&(e===`true`||e===`True`||e===`TRUE`)||t===5&&(e===`false`||e===`False`||e===`FALSE`)}function i(e){return e===`true`||e===`True`||e===`TRUE`}function a(e){return Object.prototype.toString.call(e)===`[object Boolean]`}t.exports=new n(`tag:yaml.org,2002:bool`,{kind:`scalar`,resolve:r,construct:i,predicate:a,represent:{lowercase:function(e){return e?`true`:`false`},uppercase:function(e){return e?`TRUE`:`FALSE`},camelcase:function(e){return e?`True`:`False`}},defaultStyle:`lowercase`})})),j=E(((e,t)=>{var n=fe(),r=D();function i(e){return e>=48&&e<=57||e>=65&&e<=70||e>=97&&e<=102}function a(e){return e>=48&&e<=55}function o(e){return e>=48&&e<=57}function s(e){if(e===null)return!1;let t=e.length,n=0,r=!1;if(!t)return!1;let s=e[n];if((s===`-`||s===`+`)&&(s=e[++n]),s===`0`){if(n+1===t)return!0;if(s=e[++n],s===`b`){for(n++;n<t;n++){if(s=e[n],s!==`0`&&s!==`1`)return!1;r=!0}return r&&Number.isFinite(c(e))}if(s===`x`){for(n++;n<t;n++){if(!i(e.charCodeAt(n)))return!1;r=!0}return r&&Number.isFinite(c(e))}if(s===`o`){for(n++;n<t;n++){if(!a(e.charCodeAt(n)))return!1;r=!0}return r&&Number.isFinite(c(e))}}for(;n<t;n++){if(!o(e.charCodeAt(n)))return!1;r=!0}return r?Number.isFinite(c(e)):!1}function c(e){let t=e,n=1,r=t[0];if((r===`-`||r===`+`)&&(r===`-`&&(n=-1),t=t.slice(1),r=t[0]),t===`0`)return 0;if(r===`0`){if(t[1]===`b`)return n*parseInt(t.slice(2),2);if(t[1]===`x`)return n*parseInt(t.slice(2),16);if(t[1]===`o`)return n*parseInt(t.slice(2),8)}return n*parseInt(t,10)}function l(e){return c(e)}function u(e){return Object.prototype.toString.call(e)===`[object Number]`&&e%1==0&&!n.isNegativeZero(e)}t.exports=new r(`tag:yaml.org,2002:int`,{kind:`scalar`,resolve:s,construct:l,predicate:u,represent:{binary:function(e){return e>=0?`0b`+e.toString(2):`-0b`+e.toString(2).slice(1)},octal:function(e){return e>=0?`0o`+e.toString(8):`-0o`+e.toString(8).slice(1)},decimal:function(e){return e.toString(10)},hexadecimal:function(e){return e>=0?`0x`+e.toString(16).toUpperCase():`-0x`+e.toString(16).toUpperCase().slice(1)}},defaultStyle:`decimal`,styleAliases:{binary:[2,`bin`],octal:[8,`oct`],decimal:[10,`dec`],hexadecimal:[16,`hex`]}})})),M=E(((e,t)=>{var n=fe(),r=D(),i=RegExp(`^(?:[-+]?(?:[0-9]+)(?:\\.[0-9]*)?(?:[eE][-+]?[0-9]+)?|\\.[0-9]+(?:[eE][-+]?[0-9]+)?|[-+]?\\.(?:inf|Inf|INF)|\\.(?:nan|NaN|NAN))$`),a=RegExp(`^(?:[-+]?\\.(?:inf|Inf|INF)|\\.(?:nan|NaN|NAN))$`);function o(e){return e===null||!i.test(e)?!1:Number.isFinite(parseFloat(e,10))?!0:a.test(e)}function s(e){let t=e.toLowerCase(),n=t[0]===`-`?-1:1;return`+-`.indexOf(t[0])>=0&&(t=t.slice(1)),t===`.inf`?n===1?1/0:-1/0:t===`.nan`?NaN:n*parseFloat(t,10)}var c=/^[-+]?[0-9]+e/;function l(e,t){if(isNaN(e))switch(t){case`lowercase`:return`.nan`;case`uppercase`:return`.NAN`;case`camelcase`:return`.NaN`}else if(e===1/0)switch(t){case`lowercase`:return`.inf`;case`uppercase`:return`.INF`;case`camelcase`:return`.Inf`}else if(e===-1/0)switch(t){case`lowercase`:return`-.inf`;case`uppercase`:return`-.INF`;case`camelcase`:return`-.Inf`}else if(n.isNegativeZero(e))return`-0.0`;let r=e.toString(10);return c.test(r)?r.replace(`e`,`.e`):r}function u(e){return Object.prototype.toString.call(e)===`[object Number]`&&(e%1!=0||n.isNegativeZero(e))}t.exports=new r(`tag:yaml.org,2002:float`,{kind:`scalar`,resolve:o,construct:s,predicate:u,represent:l,defaultStyle:`lowercase`})})),ye=E(((e,t)=>{t.exports=_e().extend({implicit:[A(),ve(),j(),M()]})})),be=E(((e,t)=>{t.exports=ye()})),xe=E(((e,t)=>{var n=D(),r=RegExp(`^([0-9][0-9][0-9][0-9])-([0-9][0-9])-([0-9][0-9])$`),i=RegExp(`^([0-9][0-9][0-9][0-9])-([0-9][0-9]?)-([0-9][0-9]?)(?:[Tt]|[ \\t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\\.([0-9]*))?(?:[ \\t]*(Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?$`);function a(e){return e===null?!1:r.exec(e)!==null||i.exec(e)!==null}function o(e){let t=0,n=null,a=r.exec(e);if(a===null&&(a=i.exec(e)),a===null)throw Error(`Date resolve error`);let o=+a[1],s=a[2]-1,c=+a[3];if(!a[4])return new Date(Date.UTC(o,s,c));let l=+a[4],u=+a[5],d=+a[6];if(a[7]){for(t=a[7].slice(0,3);t.length<3;)t+=`0`;t=+t}if(a[9]){let e=+a[10],t=+(a[11]||0);n=(e*60+t)*6e4,a[9]===`-`&&(n=-n)}let f=new Date(Date.UTC(o,s,c,l,u,d,t));return n&&f.setTime(f.getTime()-n),f}function s(e){return e.toISOString()}t.exports=new n(`tag:yaml.org,2002:timestamp`,{kind:`scalar`,resolve:a,construct:o,instanceOf:Date,represent:s})})),Se=E(((e,t)=>{var n=D();function r(e){return e===`<<`||e===null}t.exports=new n(`tag:yaml.org,2002:merge`,{kind:`scalar`,resolve:r})})),Ce=E(((e,t)=>{var n=D(),r=`ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=
\r`;function i(e){if(e===null)return!1;let t=0,n=e.length,i=r;for(let r=0;r<n;r++){let n=i.indexOf(e.charAt(r));if(!(n>64)){if(n<0)return!1;t+=6}}return t%8==0}function a(e){let t=e.replace(/[\r\n=]/g,``),n=t.length,i=r,a=0,o=[];for(let e=0;e<n;e++)e%4==0&&e&&(o.push(a>>16&255),o.push(a>>8&255),o.push(a&255)),a=a<<6|i.indexOf(t.charAt(e));let s=n%4*6;return s===0?(o.push(a>>16&255),o.push(a>>8&255),o.push(a&255)):s===18?(o.push(a>>10&255),o.push(a>>2&255)):s===12&&o.push(a>>4&255),new Uint8Array(o)}function o(e){let t=``,n=0,i=e.length,a=r;for(let r=0;r<i;r++)r%3==0&&r&&(t+=a[n>>18&63],t+=a[n>>12&63],t+=a[n>>6&63],t+=a[n&63]),n=(n<<8)+e[r];let o=i%3;return o===0?(t+=a[n>>18&63],t+=a[n>>12&63],t+=a[n>>6&63],t+=a[n&63]):o===2?(t+=a[n>>10&63],t+=a[n>>4&63],t+=a[n<<2&63],t+=a[64]):o===1&&(t+=a[n>>2&63],t+=a[n<<4&63],t+=a[64],t+=a[64]),t}function s(e){return Object.prototype.toString.call(e)===`[object Uint8Array]`}t.exports=new n(`tag:yaml.org,2002:binary`,{kind:`scalar`,resolve:i,construct:a,predicate:s,represent:o})})),N=E(((e,t)=>{var n=D(),r=Object.prototype.hasOwnProperty,i=Object.prototype.toString;function a(e){if(e===null)return!0;let t=[],n=e;for(let e=0,a=n.length;e<a;e+=1){let a=n[e],o=!1;if(i.call(a)!==`[object Object]`)return!1;let s;for(s in a)if(r.call(a,s)){if(!o)o=!0;else return!1}if(!o)return!1;if(t.indexOf(s)===-1)t.push(s);else return!1}return!0}function o(e){return e===null?[]:e}t.exports=new n(`tag:yaml.org,2002:omap`,{kind:`sequence`,resolve:a,construct:o})})),we=E(((e,t)=>{var n=D(),r=Object.prototype.toString;function i(e){if(e===null)return!0;let t=e,n=Array(t.length);for(let e=0,i=t.length;e<i;e+=1){let i=t[e];if(r.call(i)!==`[object Object]`)return!1;let a=Object.keys(i);if(a.length!==1)return!1;n[e]=[a[0],i[a[0]]]}return!0}function a(e){if(e===null)return[];let t=e,n=Array(t.length);for(let e=0,r=t.length;e<r;e+=1){let r=t[e],i=Object.keys(r);n[e]=[i[0],r[i[0]]]}return n}t.exports=new n(`tag:yaml.org,2002:pairs`,{kind:`sequence`,resolve:i,construct:a})})),Te=E(((e,t)=>{var n=D(),r=Object.prototype.hasOwnProperty;function i(e){if(e===null)return!0;let t=e;for(let e in t)if(r.call(t,e)&&t[e]!==null)return!1;return!0}function a(e){return e===null?{}:e}t.exports=new n(`tag:yaml.org,2002:set`,{kind:`mapping`,resolve:i,construct:a})})),Ee=E(((e,t)=>{t.exports=be().extend({implicit:[xe(),Se()],explicit:[Ce(),N(),we(),Te()]})})),De=E(((e,t)=>{var n=fe(),r=pe(),i=me(),a=Ee(),o=Object.prototype.hasOwnProperty,s=1,c=2,l=3,u=4,d=1,f=2,p=3,m=/[\x00-\x08\x0B\x0C\x0E-\x1F\x7F-\x84\x86-\x9F\uFFFE\uFFFF]|[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?:[^\uD800-\uDBFF]|^)[\uDC00-\uDFFF]/,h=/[\x85\u2028\u2029]/,g=/[,\[\]{}]/,_=/^(?:!|!!|![0-9A-Za-z-]+!)$/,v=/^(?:!|[^,\[\]{}])(?:%[0-9a-f]{2}|[0-9a-z\-#;/?:@&=+$,_.!~*'()\[\]])*$/i;function y(e){return Object.prototype.toString.call(e)}function b(e){return e===10||e===13}function x(e){return e===9||e===32}function S(e){return e===9||e===32||e===10||e===13}function C(e){return e===44||e===91||e===93||e===123||e===125}function ee(e){if(e>=48&&e<=57)return e-48;let t=e|32;return t>=97&&t<=102?t-97+10:-1}function te(e){return e===120?2:e===117?4:e===85?8:0}function ne(e){return e>=48&&e<=57?e-48:-1}function re(e){switch(e){case 48:return`\0`;case 97:return`\x07`;case 98:return`\b`;case 116:return`	`;case 9:return`	`;case 110:return`
`;case 118:return`\v`;case 102:return`\f`;case 114:return`\r`;case 101:return`\x1B`;case 32:return` `;case 34:return`"`;case 47:return`/`;case 92:return`\\`;case 78:return``;case 95:return`\xA0`;case 76:return`\u2028`;case 80:return`\u2029`;default:return``}}function ie(e){return e<=65535?String.fromCharCode(e):String.fromCharCode((e-65536>>10)+55296,(e-65536&1023)+56320)}function ae(e,t,n){t===`__proto__`?Object.defineProperty(e,t,{configurable:!0,enumerable:!0,writable:!0,value:n}):e[t]=n}var oe=Array(256),se=Array(256);for(let e=0;e<256;e++)oe[e]=+!!re(e),se[e]=re(e);function w(e,t){this.input=e,this.filename=t.filename||null,this.schema=t.schema||a,this.onWarning=t.onWarning||null,this.legacy=t.legacy||!1,this.json=t.json||!1,this.listener=t.listener||null,this.maxDepth=typeof t.maxDepth==`number`?t.maxDepth:100,this.maxMergeSeqLength=typeof t.maxMergeSeqLength==`number`?t.maxMergeSeqLength:20,this.implicitTypes=this.schema.compiledImplicit,this.typeMap=this.schema.compiledTypeMap,this.length=e.length,this.position=0,this.line=0,this.lineStart=0,this.lineIndent=0,this.depth=0,this.firstTabInLine=-1,this.documents=[],this.anchorMapTransactions=[]}function ce(e,t){let n={name:e.filename,buffer:e.input.slice(0,-1),position:e.position,line:e.line,column:e.position-e.lineStart};return n.snippet=i(n),new r(t,n)}function T(e,t){throw ce(e,t)}function le(e,t){e.onWarning&&e.onWarning.call(null,ce(e,t))}function E(e,t,n){let r=e.anchorMapTransactions;if(r.length!==0){let n=r[r.length-1];o.call(n,t)||(n[t]={existed:o.call(e.anchorMap,t),value:e.anchorMap[t]})}e.anchorMap[t]=n}function ue(e){e.anchorMapTransactions.push(Object.create(null))}function de(e){let t=e.anchorMapTransactions.pop(),n=e.anchorMapTransactions;if(n.length===0)return;let r=n[n.length-1],i=Object.keys(t);for(let e=0,n=i.length;e<n;e+=1){let n=i[e];o.call(r,n)||(r[n]=t[n])}}function D(e){let t=e.anchorMapTransactions.pop(),n=Object.keys(t);for(let r=n.length-1;r>=0;--r){let i=t[n[r]];i.existed?e.anchorMap[n[r]]=i.value:delete e.anchorMap[n[r]]}}function he(e){return{position:e.position,line:e.line,lineStart:e.lineStart,lineIndent:e.lineIndent,firstTabInLine:e.firstTabInLine,tag:e.tag,anchor:e.anchor,kind:e.kind,result:e.result}}function ge(e,t){e.position=t.position,e.line=t.line,e.lineStart=t.lineStart,e.lineIndent=t.lineIndent,e.firstTabInLine=t.firstTabInLine,e.tag=t.tag,e.anchor=t.anchor,e.kind=t.kind,e.result=t.result}var O={YAML:function(e,t,n){e.version!==null&&T(e,`duplication of %YAML directive`),n.length!==1&&T(e,`YAML directive accepts exactly one argument`);let r=/^([0-9]+)\.([0-9]+)$/.exec(n[0]);r===null&&T(e,`ill-formed argument of the YAML directive`);let i=parseInt(r[1],10),a=parseInt(r[2],10);i!==1&&T(e,`unacceptable YAML version of the document`),e.version=n[0],e.checkLineBreaks=a<2,a!==1&&a!==2&&le(e,`unsupported YAML version of the document`)},TAG:function(e,t,n){let r;n.length!==2&&T(e,`TAG directive accepts exactly two arguments`);let i=n[0];r=n[1],_.test(i)||T(e,`ill-formed tag handle (first argument) of the TAG directive`),o.call(e.tagMap,i)&&T(e,`there is a previously declared suffix for "`+i+`" tag handle`),v.test(r)||T(e,`ill-formed tag prefix (second argument) of the TAG directive`);try{r=decodeURIComponent(r)}catch{T(e,`tag prefix is malformed: `+r)}e.tagMap[i]=r}};function k(e,t,n,r){if(t<n){let i=e.input.slice(t,n);if(r)for(let t=0,n=i.length;t<n;t+=1){let n=i.charCodeAt(t);n===9||n>=32&&n<=1114111||T(e,`expected valid JSON character`)}else m.test(i)&&T(e,`the stream contains non-printable characters`);e.result+=i}}function _e(e,t,r,i){n.isObject(r)||T(e,`cannot merge mappings; the provided source object is unacceptable`);let a=Object.keys(r);for(let e=0,n=a.length;e<n;e+=1){let n=a[e];o.call(t,n)||(ae(t,n,r[n]),i[n]=!0)}}function A(e,t,n,r,i,a,s,c,l){if(Array.isArray(i)){i=Array.prototype.slice.call(i);for(let t=0,n=i.length;t<n;t+=1)Array.isArray(i[t])&&T(e,`nested arrays are not supported inside keys`),typeof i==`object`&&y(i[t])===`[object Object]`&&(i[t]=`[object Object]`)}if(typeof i==`object`&&y(i)===`[object Object]`&&(i=`[object Object]`),i=String(i),t===null&&(t={}),r===`tag:yaml.org,2002:merge`){if(Array.isArray(a)){a.length>e.maxMergeSeqLength&&T(e,`merge sequence length exceeded maxMergeSeqLength (`+e.maxMergeSeqLength+`)`);let r=new Set;for(let i=0,o=a.length;i<o;i+=1){let o=a[i];r.has(o)||(r.add(o),_e(e,t,o,n))}}else _e(e,t,a,n)}else!e.json&&!o.call(n,i)&&o.call(t,i)&&(e.line=s||e.line,e.lineStart=c||e.lineStart,e.position=l||e.position,T(e,`duplicated mapping key`)),ae(t,i,a),delete n[i];return t}function ve(e){let t=e.input.charCodeAt(e.position);t===10?e.position++:t===13?(e.position++,e.input.charCodeAt(e.position)===10&&e.position++):T(e,`a line break is expected`),e.line+=1,e.lineStart=e.position,e.firstTabInLine=-1}function j(e,t,n){let r=0,i=e.input.charCodeAt(e.position);for(;i!==0;){for(;x(i);)i===9&&e.firstTabInLine===-1&&(e.firstTabInLine=e.position),i=e.input.charCodeAt(++e.position);if(t&&i===35)do i=e.input.charCodeAt(++e.position);while(i!==10&&i!==13&&i!==0);if(b(i))for(ve(e),i=e.input.charCodeAt(e.position),r++,e.lineIndent=0;i===32;)e.lineIndent++,i=e.input.charCodeAt(++e.position);else break}return n!==-1&&r!==0&&e.lineIndent<n&&le(e,`deficient indentation`),r}function M(e){let t=e.position,n=e.input.charCodeAt(t);return!!((n===45||n===46)&&n===e.input.charCodeAt(t+1)&&n===e.input.charCodeAt(t+2)&&(t+=3,n=e.input.charCodeAt(t),n===0||S(n)))}function ye(e,t){t===1?e.result+=` `:t>1&&(e.result+=n.repeat(`
`,t-1))}function be(e,t,n){let r,i,a,o,s,c,l=e.kind,u=e.result,d=e.input.charCodeAt(e.position);if(S(d)||C(d)||d===35||d===38||d===42||d===33||d===124||d===62||d===39||d===34||d===37||d===64||d===96)return!1;if(d===63||d===45){let t=e.input.charCodeAt(e.position+1);if(S(t)||n&&C(t))return!1}for(e.kind=`scalar`,e.result=``,r=i=e.position,a=!1;d!==0;){if(d===58){let t=e.input.charCodeAt(e.position+1);if(S(t)||n&&C(t))break}else if(d===35){if(S(e.input.charCodeAt(e.position-1)))break}else if(e.position===e.lineStart&&M(e)||n&&C(d))break;else if(b(d)){if(o=e.line,s=e.lineStart,c=e.lineIndent,j(e,!1,-1),e.lineIndent>=t){a=!0,d=e.input.charCodeAt(e.position);continue}e.position=i,e.line=o,e.lineStart=s,e.lineIndent=c;break}a&&=(k(e,r,i,!1),ye(e,e.line-o),r=i=e.position,!1),x(d)||(i=e.position+1),d=e.input.charCodeAt(++e.position)}return k(e,r,i,!1),e.result?!0:(e.kind=l,e.result=u,!1)}function xe(e,t){let n,r,i=e.input.charCodeAt(e.position);if(i!==39)return!1;for(e.kind=`scalar`,e.result=``,e.position++,n=r=e.position;(i=e.input.charCodeAt(e.position))!==0;)if(i===39){if(k(e,n,e.position,!0),i=e.input.charCodeAt(++e.position),i===39)n=e.position,e.position++,r=e.position;else return!0}else b(i)?(k(e,n,r,!0),ye(e,j(e,!1,t)),n=r=e.position):e.position===e.lineStart&&M(e)?T(e,`unexpected end of the document within a single quoted scalar`):(e.position++,x(i)||(r=e.position));T(e,`unexpected end of the stream within a single quoted scalar`)}function Se(e,t){let n,r,i,a=e.input.charCodeAt(e.position);if(a!==34)return!1;for(e.kind=`scalar`,e.result=``,e.position++,n=r=e.position;(a=e.input.charCodeAt(e.position))!==0;)if(a===34)return k(e,n,e.position,!0),e.position++,!0;else if(a===92){if(k(e,n,e.position,!0),a=e.input.charCodeAt(++e.position),b(a))j(e,!1,t);else if(a<256&&oe[a])e.result+=se[a],e.position++;else if((i=te(a))>0){let t=i,n=0;for(;t>0;t--)a=e.input.charCodeAt(++e.position),(i=ee(a))>=0?n=(n<<4)+i:T(e,`expected hexadecimal character`);e.result+=ie(n),e.position++}else T(e,`unknown escape sequence`);n=r=e.position}else b(a)?(k(e,n,r,!0),ye(e,j(e,!1,t)),n=r=e.position):e.position===e.lineStart&&M(e)?T(e,`unexpected end of the document within a double quoted scalar`):(e.position++,x(a)||(r=e.position));T(e,`unexpected end of the stream within a double quoted scalar`)}function Ce(e,t){let n=!0,r,i,a,o=e.tag,c,l=e.anchor,u,d,f,p,m=Object.create(null),h,g,_,v=e.input.charCodeAt(e.position);if(v===91)u=93,p=!1,c=[];else if(v===123)u=125,p=!0,c={};else return!1;for(e.anchor!==null&&E(e,e.anchor,c),v=e.input.charCodeAt(++e.position);v!==0;){if(j(e,!0,t),v=e.input.charCodeAt(e.position),v===u)return e.position++,e.tag=o,e.anchor=l,e.kind=p?`mapping`:`sequence`,e.result=c,!0;n?v===44&&T(e,`expected the node content, but found ','`):T(e,`missed comma between flow collection entries`),g=h=_=null,d=f=!1,v===63&&S(e.input.charCodeAt(e.position+1))&&(d=f=!0,e.position++,j(e,!0,t)),r=e.line,i=e.lineStart,a=e.position,je(e,t,s,!1,!0),g=e.tag,h=e.result,j(e,!0,t),v=e.input.charCodeAt(e.position),(f||e.line===r)&&v===58&&(d=!0,v=e.input.charCodeAt(++e.position),j(e,!0,t),je(e,t,s,!1,!0),_=e.result),p?A(e,c,m,g,h,_,r,i,a):d?c.push(A(e,null,m,g,h,_,r,i,a)):c.push(h),j(e,!0,t),v=e.input.charCodeAt(e.position),v===44?(n=!0,v=e.input.charCodeAt(++e.position)):n=!1}T(e,`unexpected end of the stream within a flow collection`)}function N(e,t){let r,i=d,a=!1,o=!1,s=t,c=0,l=!1,u,m=e.input.charCodeAt(e.position);if(m===124)r=!1;else if(m===62)r=!0;else return!1;for(e.kind=`scalar`,e.result=``;m!==0;)if(m=e.input.charCodeAt(++e.position),m===43||m===45)d===i?i=m===43?p:f:T(e,`repeat of a chomping mode identifier`);else if((u=ne(m))>=0)u===0?T(e,`bad explicit indentation width of a block scalar; it cannot be less than one`):o?T(e,`repeat of an indentation width identifier`):(s=t+u-1,o=!0);else break;if(x(m)){do m=e.input.charCodeAt(++e.position);while(x(m));if(m===35)do m=e.input.charCodeAt(++e.position);while(!b(m)&&m!==0)}for(;m!==0;){for(ve(e),e.lineIndent=0,m=e.input.charCodeAt(e.position);(!o||e.lineIndent<s)&&m===32;)e.lineIndent++,m=e.input.charCodeAt(++e.position);if(!o&&e.lineIndent>s&&(s=e.lineIndent),b(m)){c++;continue}if(!o&&s===0&&T(e,`missing indentation for block scalar`),e.lineIndent<s){i===p?e.result+=n.repeat(`
`,a?1+c:c):i===d&&a&&(e.result+=`
`);break}r?x(m)?(l=!0,e.result+=n.repeat(`
`,a?1+c:c)):l?(l=!1,e.result+=n.repeat(`
`,c+1)):c===0?a&&(e.result+=` `):e.result+=n.repeat(`
`,c):e.result+=n.repeat(`
`,a?1+c:c),a=!0,o=!0,c=0;let t=e.position;for(;!b(m)&&m!==0;)m=e.input.charCodeAt(++e.position);k(e,t,e.position,!1)}return!0}function we(e,t){let n=e.tag,r=e.anchor,i=[],a=!1;if(e.firstTabInLine!==-1)return!1;e.anchor!==null&&E(e,e.anchor,i);let o=e.input.charCodeAt(e.position);for(;o!==0&&(e.firstTabInLine!==-1&&(e.position=e.firstTabInLine,T(e,`tab characters must not be used in indentation`)),o===45&&S(e.input.charCodeAt(e.position+1)));){if(a=!0,e.position++,j(e,!0,-1)&&e.lineIndent<=t){i.push(null),o=e.input.charCodeAt(e.position);continue}let n=e.line;if(je(e,t,l,!1,!0),i.push(e.result),j(e,!0,-1),o=e.input.charCodeAt(e.position),(e.line===n||e.lineIndent>t)&&o!==0)T(e,`bad indentation of a sequence entry`);else if(e.lineIndent<t)break}return a?(e.tag=n,e.anchor=r,e.kind=`sequence`,e.result=i,!0):!1}function Te(e,t,n){let r,i,a,o,s=e.tag,l=e.anchor,d={},f=Object.create(null),p=null,m=null,h=null,g=!1,_=!1;if(e.firstTabInLine!==-1)return!1;e.anchor!==null&&E(e,e.anchor,d);let v=e.input.charCodeAt(e.position);for(;v!==0;){!g&&e.firstTabInLine!==-1&&(e.position=e.firstTabInLine,T(e,`tab characters must not be used in indentation`));let y=e.input.charCodeAt(e.position+1),b=e.line;if((v===63||v===58)&&S(y))v===63?(g&&(A(e,d,f,p,m,null,i,a,o),p=m=h=null),_=!0,g=!0,r=!0):g?(g=!1,r=!0):T(e,`incomplete explicit mapping pair; a key node is missed; or followed by a non-tabulated empty line`),e.position+=1,v=y;else{if(i=e.line,a=e.lineStart,o=e.position,!je(e,n,c,!1,!0))break;if(e.line===b){for(v=e.input.charCodeAt(e.position);x(v);)v=e.input.charCodeAt(++e.position);if(v===58)v=e.input.charCodeAt(++e.position),S(v)||T(e,`a whitespace character is expected after the key-value separator within a block mapping`),g&&(A(e,d,f,p,m,null,i,a,o),p=m=h=null),_=!0,g=!1,r=!1,p=e.tag,m=e.result;else if(_)T(e,`can not read an implicit mapping pair; a colon is missed`);else return e.tag=s,e.anchor=l,!0}else if(_)T(e,`can not read a block mapping entry; a multiline key may not be an implicit key`);else return e.tag=s,e.anchor=l,!0}if((e.line===b||e.lineIndent>t)&&(g&&(i=e.line,a=e.lineStart,o=e.position),je(e,t,u,!0,r)&&(g?m=e.result:h=e.result),g||(A(e,d,f,p,m,h,i,a,o),p=m=h=null),j(e,!0,-1),v=e.input.charCodeAt(e.position)),(e.line===b||e.lineIndent>t)&&v!==0)T(e,`bad indentation of a mapping entry`);else if(e.lineIndent<t)break}return g&&A(e,d,f,p,m,null,i,a,o),_&&(e.tag=s,e.anchor=l,e.kind=`mapping`,e.result=d),_}function De(e){let t=!1,n=!1,r,i,a=e.input.charCodeAt(e.position);if(a!==33)return!1;e.tag!==null&&T(e,`duplication of a tag property`),a=e.input.charCodeAt(++e.position),a===60?(t=!0,a=e.input.charCodeAt(++e.position)):a===33?(n=!0,r=`!!`,a=e.input.charCodeAt(++e.position)):r=`!`;let s=e.position;if(t){do a=e.input.charCodeAt(++e.position);while(a!==0&&a!==62);e.position<e.length?(i=e.input.slice(s,e.position),a=e.input.charCodeAt(++e.position)):T(e,`unexpected end of the stream within a verbatim tag`)}else{for(;a!==0&&!S(a);)a===33&&(n?T(e,`tag suffix cannot contain exclamation marks`):(r=e.input.slice(s-1,e.position+1),_.test(r)||T(e,`named tag handle cannot contain such characters`),n=!0,s=e.position+1)),a=e.input.charCodeAt(++e.position);i=e.input.slice(s,e.position),g.test(i)&&T(e,`tag suffix cannot contain flow indicator characters`)}i&&!v.test(i)&&T(e,`tag name cannot contain such characters: `+i);try{i=decodeURIComponent(i)}catch{T(e,`tag name is malformed: `+i)}return t?e.tag=i:o.call(e.tagMap,r)?e.tag=e.tagMap[r]+i:r===`!`?e.tag=`!`+i:r===`!!`?e.tag=`tag:yaml.org,2002:`+i:T(e,`undeclared tag handle "`+r+`"`),!0}function Oe(e){let t=e.input.charCodeAt(e.position);if(t!==38)return!1;e.anchor!==null&&T(e,`duplication of an anchor property`),t=e.input.charCodeAt(++e.position);let n=e.position;for(;t!==0&&!S(t)&&!C(t);)t=e.input.charCodeAt(++e.position);return e.position===n&&T(e,`name of an anchor node must contain at least one character`),e.anchor=e.input.slice(n,e.position),!0}function ke(e){let t=e.input.charCodeAt(e.position);if(t!==42)return!1;t=e.input.charCodeAt(++e.position);let n=e.position;for(;t!==0&&!S(t)&&!C(t);)t=e.input.charCodeAt(++e.position);e.position===n&&T(e,`name of an alias node must contain at least one character`);let r=e.input.slice(n,e.position);return o.call(e.anchorMap,r)||T(e,`unidentified alias "`+r+`"`),e.result=e.anchorMap[r],j(e,!0,-1),!0}function Ae(e,t,n,r){let i=he(e);return ue(e),ge(e,t),e.tag=null,e.anchor=null,e.kind=null,e.result=null,Te(e,n,r)&&e.kind===`mapping`?(de(e),!0):(D(e),ge(e,i),!1)}function je(e,t,n,r,i){let a,d,f=1,p=!1,m=!1,h=null,g,_,v;e.depth>=e.maxDepth&&T(e,`nesting exceeded maxDepth (`+e.maxDepth+`)`),e.depth+=1,e.listener!==null&&e.listener(`open`,e),e.tag=null,e.anchor=null,e.kind=null,e.result=null;let y=a=d=u===n||l===n;if(r&&j(e,!0,-1)&&(p=!0,e.lineIndent>t?f=1:e.lineIndent===t?f=0:e.lineIndent<t&&(f=-1)),f===1)for(;;){let n=e.input.charCodeAt(e.position),r=he(e);if(p&&(n===33&&e.tag!==null||n===38&&e.anchor!==null)||!De(e)&&!Oe(e))break;h===null&&(h=r),j(e,!0,-1)?(p=!0,d=y,e.lineIndent>t?f=1:e.lineIndent===t?f=0:e.lineIndent<t&&(f=-1)):d=!1}if(d&&=p||i,f===1||u===n){if(_=s===n||c===n?t:t+1,v=e.position-e.lineStart,f===1){if(d&&(we(e,v)||Te(e,v,_))||Ce(e,_))m=!0;else{let t=e.input.charCodeAt(e.position);h!==null&&y&&!d&&t!==124&&t!==62&&Ae(e,h,h.position-h.lineStart,_)||a&&N(e,_)||xe(e,_)||Se(e,_)?m=!0:ke(e)?(m=!0,(e.tag!==null||e.anchor!==null)&&T(e,`alias node should not have any properties`)):be(e,_,s===n)&&(m=!0,e.tag===null&&(e.tag=`?`)),e.anchor!==null&&E(e,e.anchor,e.result)}}else f===0&&(m=d&&we(e,v))}if(e.tag===null)e.anchor!==null&&E(e,e.anchor,e.result);else if(e.tag===`?`){e.result!==null&&e.kind!==`scalar`&&T(e,`unacceptable node kind for !<?> tag; it should be "scalar", not "`+e.kind+`"`);for(let t=0,n=e.implicitTypes.length;t<n;t+=1)if(g=e.implicitTypes[t],g.resolve(e.result)){e.result=g.construct(e.result),e.tag=g.tag,e.anchor!==null&&E(e,e.anchor,e.result);break}}else if(e.tag!==`!`){if(o.call(e.typeMap[e.kind||`fallback`],e.tag))g=e.typeMap[e.kind||`fallback`][e.tag];else{g=null;let t=e.typeMap.multi[e.kind||`fallback`];for(let n=0,r=t.length;n<r;n+=1)if(e.tag.slice(0,t[n].tag.length)===t[n].tag){g=t[n];break}}g||T(e,`unknown tag !<`+e.tag+`>`),e.result!==null&&g.kind!==e.kind&&T(e,`unacceptable node kind for !<`+e.tag+`> tag; it should be "`+g.kind+`", not "`+e.kind+`"`),g.resolve(e.result,e.tag)?(e.result=g.construct(e.result,e.tag),e.anchor!==null&&E(e,e.anchor,e.result)):T(e,`cannot resolve a node with !<`+e.tag+`> explicit tag`)}return e.listener!==null&&e.listener(`close`,e),--e.depth,e.tag!==null||e.anchor!==null||m}function Me(e){let t=e.position,n=!1,r;for(e.version=null,e.checkLineBreaks=e.legacy,e.tagMap=Object.create(null),e.anchorMap=Object.create(null);(r=e.input.charCodeAt(e.position))!==0&&(j(e,!0,-1),r=e.input.charCodeAt(e.position),!(e.lineIndent>0||r!==37));){n=!0,r=e.input.charCodeAt(++e.position);let t=e.position;for(;r!==0&&!S(r);)r=e.input.charCodeAt(++e.position);let i=e.input.slice(t,e.position),a=[];for(i.length<1&&T(e,`directive name must not be less than one character in length`);r!==0;){for(;x(r);)r=e.input.charCodeAt(++e.position);if(r===35){do r=e.input.charCodeAt(++e.position);while(r!==0&&!b(r));break}if(b(r))break;for(t=e.position;r!==0&&!S(r);)r=e.input.charCodeAt(++e.position);a.push(e.input.slice(t,e.position))}r!==0&&ve(e),o.call(O,i)?O[i](e,i,a):le(e,`unknown document directive "`+i+`"`)}j(e,!0,-1),e.lineIndent===0&&e.input.charCodeAt(e.position)===45&&e.input.charCodeAt(e.position+1)===45&&e.input.charCodeAt(e.position+2)===45?(e.position+=3,j(e,!0,-1)):n&&T(e,`directives end mark is expected`),je(e,e.lineIndent-1,u,!1,!0),j(e,!0,-1),e.checkLineBreaks&&h.test(e.input.slice(t,e.position))&&le(e,`non-ASCII line breaks are interpreted as content`),e.documents.push(e.result),e.position===e.lineStart&&M(e)?e.input.charCodeAt(e.position)===46&&(e.position+=3,j(e,!0,-1)):e.position<e.length-1&&T(e,`end of the stream or a document separator is expected`)}function Ne(e,t){e=String(e),t||={},e.length!==0&&(e.charCodeAt(e.length-1)!==10&&e.charCodeAt(e.length-1)!==13&&(e+=`
`),e.charCodeAt(0)===65279&&(e=e.slice(1)));let n=new w(e,t),r=e.indexOf(`\0`);for(r!==-1&&(n.position=r,T(n,`null byte is not allowed in input`)),n.input+=`\0`;n.input.charCodeAt(n.position)===32;)n.lineIndent+=1,n.position+=1;for(;n.position<n.length-1;)Me(n);return n.documents}function Pe(e,t,n){typeof t==`object`&&t&&n===void 0&&(n=t,t=null);let r=Ne(e,n);if(typeof t!=`function`)return r;for(let e=0,n=r.length;e<n;e+=1)t(r[e])}function Fe(e,t){let n=Ne(e,t);if(n.length!==0){if(n.length===1)return n[0];throw new r(`expected a single document in the stream, but found more`)}}t.exports.loadAll=Pe,t.exports.load=Fe})),Oe=E(((e,t)=>{var n=fe(),r=pe(),i=Ee(),a=Object.prototype.toString,o=Object.prototype.hasOwnProperty,s=65279,c=9,l=10,u=13,d=32,f=33,p=34,m=35,h=37,g=38,_=39,v=42,y=44,b=45,x=58,S=61,C=62,ee=63,te=64,ne=91,re=93,ie=96,ae=123,oe=124,se=125,w={};w[0]=`\\0`,w[7]=`\\a`,w[8]=`\\b`,w[9]=`\\t`,w[10]=`\\n`,w[11]=`\\v`,w[12]=`\\f`,w[13]=`\\r`,w[27]=`\\e`,w[34]=`\\"`,w[92]=`\\\\`,w[133]=`\\N`,w[160]=`\\_`,w[8232]=`\\L`,w[8233]=`\\P`;var ce=[`y`,`Y`,`yes`,`Yes`,`YES`,`on`,`On`,`ON`,`n`,`N`,`no`,`No`,`NO`,`off`,`Off`,`OFF`],T=/^[-+]?[0-9_]+(?::[0-9_]+)+(?:\.[0-9_]*)?$/;function le(e,t){if(t===null)return{};let n={},r=Object.keys(t);for(let i=0,a=r.length;i<a;i+=1){let a=r[i],s=String(t[a]);a.slice(0,2)===`!!`&&(a=`tag:yaml.org,2002:`+a.slice(2));let c=e.compiledTypeMap.fallback[a];c&&o.call(c.styleAliases,s)&&(s=c.styleAliases[s]),n[a]=s}return n}function E(e){let t,i,a=e.toString(16).toUpperCase();if(e<=255)t=`x`,i=2;else if(e<=65535)t=`u`,i=4;else if(e<=4294967295)t=`U`,i=8;else throw new r(`code point within a string may not be greater than 0xFFFFFFFF`);return`\\`+t+n.repeat(`0`,i-a.length)+a}var ue=1,de=2;function me(e){this.schema=e.schema||i,this.indent=Math.max(1,e.indent||2),this.noArrayIndent=e.noArrayIndent||!1,this.skipInvalid=e.skipInvalid||!1,this.flowLevel=n.isNothing(e.flowLevel)?-1:e.flowLevel,this.styleMap=le(this.schema,e.styles||null),this.sortKeys=e.sortKeys||!1,this.lineWidth=e.lineWidth||80,this.noRefs=e.noRefs||!1,this.noCompatMode=e.noCompatMode||!1,this.condenseFlow=e.condenseFlow||!1,this.quotingType=e.quotingType===`"`?de:ue,this.forceQuotes=e.forceQuotes||!1,this.replacer=typeof e.replacer==`function`?e.replacer:null,this.implicitTypes=this.schema.compiledImplicit,this.explicitTypes=this.schema.compiledExplicit,this.tag=null,this.result=``,this.duplicates=[],this.usedDuplicates=null}function D(e,t){let r=n.repeat(` `,t),i=0,a=``,o=e.length;for(;i<o;){let t,n=e.indexOf(`
`,i);n===-1?(t=e.slice(i),i=o):(t=e.slice(i,n+1),i=n+1),t.length&&t!==`
`&&(a+=r),a+=t}return a}function he(e,t){return`
`+n.repeat(` `,e.indent*t)}function ge(e,t){for(let n=0,r=e.implicitTypes.length;n<r;n+=1)if(e.implicitTypes[n].resolve(t))return!0;return!1}function O(e){return e===d||e===c}function k(e){return e>=32&&e<=126||e>=161&&e<=55295&&e!==8232&&e!==8233||e>=57344&&e<=65533&&e!==s||e>=65536&&e<=1114111}function _e(e){return k(e)&&e!==s&&e!==u&&e!==l}function A(e,t,n){let r=_e(e),i=r&&!O(e);return(n?r:r&&e!==y&&e!==ne&&e!==re&&e!==ae&&e!==se)&&e!==m&&!(t===x&&!i)||_e(t)&&!O(t)&&e===m||t===x&&i}function ve(e){return k(e)&&e!==s&&!O(e)&&e!==b&&e!==ee&&e!==x&&e!==y&&e!==ne&&e!==re&&e!==ae&&e!==se&&e!==m&&e!==g&&e!==v&&e!==f&&e!==oe&&e!==S&&e!==C&&e!==_&&e!==p&&e!==h&&e!==te&&e!==ie}function j(e){return!O(e)&&e!==x}function M(e,t){let n=e.charCodeAt(t),r;return n>=55296&&n<=56319&&t+1<e.length&&(r=e.charCodeAt(t+1),r>=56320&&r<=57343)?(n-55296)*1024+r-56320+65536:n}function ye(e){return/^\n* /.test(e)}var be=1,xe=2,Se=3,Ce=4,N=5;function we(e,t,n,r,i,a,o,s){let c,u=0,d=null,f=!1,p=!1,m=r!==-1,h=-1,g=ve(M(e,0))&&j(M(e,e.length-1));if(t||o)for(c=0;c<e.length;u>=65536?c+=2:c++){if(u=M(e,c),!k(u))return N;g&&=A(u,d,s),d=u}else{for(c=0;c<e.length;u>=65536?c+=2:c++){if(u=M(e,c),u===l)f=!0,m&&(p||=c-h-1>r&&e[h+1]!==` `,h=c);else if(!k(u))return N;g&&=A(u,d,s),d=u}p||=m&&c-h-1>r&&e[h+1]!==` `}return!f&&!p?g&&!o&&!i(e)?be:a===de?N:xe:n>9&&ye(e)?N:o?a===de?N:xe:p?Ce:Se}function Te(e,t,n,i,a){e.dump=function(){if(t.length===0)return e.quotingType===de?`""`:`''`;if(!e.noCompatMode&&(ce.indexOf(t)!==-1||T.test(t)))return e.quotingType===de?`"`+t+`"`:`'`+t+`'`;let o=e.indent*Math.max(1,n),s=e.lineWidth===-1?-1:Math.max(Math.min(e.lineWidth,40),e.lineWidth-o),c=i||e.flowLevel>-1&&n>=e.flowLevel;function l(t){return ge(e,t)}switch(we(t,c,e.indent,s,l,e.quotingType,e.forceQuotes&&!i,a)){case be:return t;case xe:return`'`+t.replace(/'/g,`''`)+`'`;case Se:return`|`+De(t,e.indent)+Oe(D(t,o));case Ce:return`>`+De(t,e.indent)+Oe(D(ke(t,s),o));case N:return`"`+je(t,s)+`"`;default:throw new r(`impossible error: invalid scalar style`)}}()}function De(e,t){let n=ye(e)?String(t):``,r=e[e.length-1]===`
`;return n+(r&&(e[e.length-2]===`
`||e===`
`)?`+`:r?``:`-`)+`
`}function Oe(e){return e[e.length-1]===`
`?e.slice(0,-1):e}function ke(e,t){let n=/(\n+)([^\n]*)/g,r=function(){let r=e.indexOf(`
`);return r=r===-1?e.length:r,n.lastIndex=r,Ae(e.slice(0,r),t)}(),i=e[0]===`
`||e[0]===` `,a,o;for(;o=n.exec(e);){let e=o[1],n=o[2];a=n[0]===` `,r+=e+(!i&&!a&&n!==``?`
`:``)+Ae(n,t),i=a}return r}function Ae(e,t){if(e===``||e[0]===` `)return e;let n=/ [^ ]/g,r,i=0,a,o=0,s=0,c=``;for(;r=n.exec(e);)s=r.index,s-i>t&&(a=o>i?o:s,c+=`
`+e.slice(i,a),i=a+1),o=s;return c+=`
`,e.length-i>t&&o>i?c+=e.slice(i,o)+`
`+e.slice(o+1):c+=e.slice(i),c.slice(1)}function je(e){let t=``,n=0;for(let r=0;r<e.length;n>=65536?r+=2:r++){n=M(e,r);let i=w[n];!i&&k(n)?(t+=e[r],n>=65536&&(t+=e[r+1])):t+=i||E(n)}return t}function Me(e,t,n){let r=``,i=e.tag;for(let i=0,a=n.length;i<a;i+=1){let a=n[i];e.replacer&&(a=e.replacer.call(n,String(i),a)),(P(e,t,a,!1,!1)||a===void 0&&P(e,t,null,!1,!1))&&(r!==``&&(r+=`,`+(e.condenseFlow?``:` `)),r+=e.dump)}e.tag=i,e.dump=`[`+r+`]`}function Ne(e,t,n,r){let i=``,a=e.tag;for(let a=0,o=n.length;a<o;a+=1){let o=n[a];e.replacer&&(o=e.replacer.call(n,String(a),o)),(P(e,t+1,o,!0,!0,!1,!0)||o===void 0&&P(e,t+1,null,!0,!0,!1,!0))&&((!r||i!==``)&&(i+=he(e,t)),e.dump&&l===e.dump.charCodeAt(0)?i+=`-`:i+=`- `,i+=e.dump)}e.tag=a,e.dump=i||`[]`}function Pe(e,t,n){let r=``,i=e.tag,a=Object.keys(n);for(let i=0,o=a.length;i<o;i+=1){let o=``;r!==``&&(o+=`, `),e.condenseFlow&&(o+=`"`);let s=a[i],c=n[s];e.replacer&&(c=e.replacer.call(n,s,c)),P(e,t,s,!1,!1)&&(e.dump.length>1024&&(o+=`? `),o+=e.dump+(e.condenseFlow?`"`:``)+`:`+(e.condenseFlow?``:` `),P(e,t,c,!1,!1)&&(o+=e.dump,r+=o))}e.tag=i,e.dump=`{`+r+`}`}function Fe(e,t,n,i){let a=``,o=e.tag,s=Object.keys(n);if(e.sortKeys===!0)s.sort();else if(typeof e.sortKeys==`function`)s.sort(e.sortKeys);else if(e.sortKeys)throw new r(`sortKeys must be a boolean or a function`);for(let r=0,o=s.length;r<o;r+=1){let o=``;(!i||a!==``)&&(o+=he(e,t));let c=s[r],u=n[c];if(e.replacer&&(u=e.replacer.call(n,c,u)),!P(e,t+1,c,!0,!0,!0))continue;let d=e.tag!==null&&e.tag!==`?`||e.dump&&e.dump.length>1024;d&&(e.dump&&l===e.dump.charCodeAt(0)?o+=`?`:o+=`? `),o+=e.dump,d&&(o+=he(e,t)),P(e,t+1,u,!0,d)&&(e.dump&&l===e.dump.charCodeAt(0)?o+=`:`:o+=`: `,o+=e.dump,a+=o)}e.tag=o,e.dump=a||`{}`}function Ie(e,t,n){let i=n?e.explicitTypes:e.implicitTypes;for(let s=0,c=i.length;s<c;s+=1){let c=i[s];if((c.instanceOf||c.predicate)&&(!c.instanceOf||typeof t==`object`&&t instanceof c.instanceOf)&&(!c.predicate||c.predicate(t))){if(e.tag=n?c.multi&&c.representName?c.representName(t):c.tag:`?`,c.represent){let n=e.styleMap[c.tag]||c.defaultStyle,i;if(a.call(c.represent)===`[object Function]`)i=c.represent(t,n);else if(o.call(c.represent,n))i=c.represent[n](t,n);else throw new r(`!<`+c.tag+`> tag resolver accepts not "`+n+`" style`);e.dump=i}return!0}}return!1}function P(e,t,n,i,o,s,c){e.tag=null,e.dump=n,Ie(e,n,!1)||Ie(e,n,!0);let l=a.call(e.dump),u=i;i&&=e.flowLevel<0||e.flowLevel>t;let d=l===`[object Object]`||l===`[object Array]`,f,p;if(d&&(f=e.duplicates.indexOf(n),p=f!==-1),(e.tag!==null&&e.tag!==`?`||p||e.indent!==2&&t>0)&&(o=!1),p&&e.usedDuplicates[f])e.dump=`*ref_`+f;else{if(d&&p&&!e.usedDuplicates[f]&&(e.usedDuplicates[f]=!0),l===`[object Object]`)i&&Object.keys(e.dump).length!==0?(Fe(e,t,e.dump,o),p&&(e.dump=`&ref_`+f+e.dump)):(Pe(e,t,e.dump),p&&(e.dump=`&ref_`+f+` `+e.dump));else if(l===`[object Array]`)i&&e.dump.length!==0?(e.noArrayIndent&&!c&&t>0?Ne(e,t-1,e.dump,o):Ne(e,t,e.dump,o),p&&(e.dump=`&ref_`+f+e.dump)):(Me(e,t,e.dump),p&&(e.dump=`&ref_`+f+` `+e.dump));else if(l===`[object String]`)e.tag!==`?`&&Te(e,e.dump,t,s,u);else if(l===`[object Undefined]`)return!1;else{if(e.skipInvalid)return!1;throw new r(`unacceptable kind of an object to dump `+l)}if(e.tag!==null&&e.tag!==`?`){let t=encodeURI(e.tag[0]===`!`?e.tag.slice(1):e.tag).replace(/!/g,`%21`);t=e.tag[0]===`!`?`!`+t:t.slice(0,18)===`tag:yaml.org,2002:`?`!!`+t.slice(18):`!<`+t+`>`,e.dump=t+` `+e.dump}}return!0}function Le(e,t){let n=[],r=[];Re(e,n,r);let i=r.length;for(let e=0;e<i;e+=1)t.duplicates.push(n[r[e]]);t.usedDuplicates=Array(i)}function Re(e,t,n){if(typeof e==`object`&&e){let r=t.indexOf(e);if(r!==-1)n.indexOf(r)===-1&&n.push(r);else if(t.push(e),Array.isArray(e))for(let r=0,i=e.length;r<i;r+=1)Re(e[r],t,n);else{let r=Object.keys(e);for(let i=0,a=r.length;i<a;i+=1)Re(e[r[i]],t,n)}}}function ze(e,t){t||={};let n=new me(t);n.noRefs||Le(e,n);let r=e;return n.replacer&&(r=n.replacer.call({"":r},``,r)),P(n,0,r,!0,!0)?n.dump+`
`:``}t.exports.dump=ze})),ke=de(E(((e,t)=>{var n=De(),r=Oe();function i(e,t){return function(){throw Error(`Function yaml.`+e+` is removed in js-yaml 4. Use yaml.`+t+` instead, which is now safe by default.`)}}t.exports.Type=D(),t.exports.Schema=he(),t.exports.FAILSAFE_SCHEMA=_e(),t.exports.JSON_SCHEMA=ye(),t.exports.CORE_SCHEMA=be(),t.exports.DEFAULT_SCHEMA=Ee(),t.exports.load=n.load,t.exports.loadAll=n.loadAll,t.exports.dump=r.dump,t.exports.YAMLException=pe(),t.exports.types={binary:Ce(),float:M(),map:k(),null:A(),pairs:we(),set:Te(),timestamp:xe(),bool:ve(),int:j(),merge:Se(),omap:N(),seq:O(),str:ge()},t.exports.safeLoad=i(`safeLoad`,`load`),t.exports.safeLoadAll=i(`safeLoadAll`,`loadAll`),t.exports.safeDump=i(`safeDump`,`dump`)}))(),1),{Type:Ae,Schema:je,FAILSAFE_SCHEMA:Me,JSON_SCHEMA:Ne,CORE_SCHEMA:Pe,DEFAULT_SCHEMA:Fe,load:Ie,loadAll:P,dump:Le,YAMLException:Re,types:ze,safeLoad:Be,safeLoadAll:Ve,safeDump:He}=ke.default,Ue=ke.default;function We(e){let t=Object.values(e).filter(e=>typeof e==`number`);return Object.entries(e).filter(([e,n])=>t.indexOf(+e)===-1).map(([e,t])=>t)}function Ge(e,t=`|`){return e.map(e=>pt(e)).join(t)}function Ke(e,t){return typeof t==`bigint`?t.toString():t}var qe=class{constructor(e){this._getter=e,this._value=void 0}get value(){let e=this._getter;return e!==void 0&&(this._value=e(),this._getter=void 0),this._value}};function Je(e){return new qe(e)}function Ye(e){return e==null}function Xe(e){let t=+!!e.startsWith(`^`),n=e.endsWith(`$`)?e.length-1:e.length;return e.slice(t,n)}function Ze(e,t){let n=e/t,r=Math.round(n),i=4*2**-52*Math.max(Math.abs(n),1);return Math.abs(n-r)<i?0:n-r}function Qe(e,t,n){Object.defineProperty(e,t,{value:n,writable:!0,enumerable:!0,configurable:!0})}function $e(e){let t=Object.getOwnPropertyDescriptor(e,`shape`);return t?.get?t.get.raw:t?.value}function F(e){return $e(e._zod.def)??e._zod.def.shape}function et(e,t,n){Object.defineProperty(e,t,{get(){let e=n();return Qe(this,t,e),e},enumerable:!0,configurable:!0})}function tt(e,t,n){t in e?Qe(e,t,n):e[t]=n}function nt(e,t,n,r){let i=F(t);for(let a of n){let n=Object.getOwnPropertyDescriptor(i,a);n.enumerable&&(n.get?et(e,a,()=>{let e=t._zod.def.shape[a];return r?r(e,a):e}):tt(e,a,r?r(n.value,a):n.value))}}function rt(e,t){for(let n of Reflect.ownKeys(t)){let r=Object.getOwnPropertyDescriptor(t,n);r.enumerable&&(r.get?et(e,n,()=>t[n]):tt(e,n,r.value))}}function I(...e){let t={};for(let n of e){let e=Object.getOwnPropertyDescriptors(n);Object.assign(t,e)}return Object.defineProperties({},t)}function it(e){return JSON.stringify(e)}function at(e){return e.toLowerCase().trim().replace(/[^\w\s-]/g,``).replace(/[\s_-]+/g,`-`).replace(/^-+|-+$/g,``)}var ot=`captureStackTrace`in Error?Error.captureStackTrace:(...e)=>{};function st(e){return typeof e==`object`&&!!e&&!Array.isArray(e)}var ct=Je(()=>{if(U.jitless||typeof navigator<`u`&&navigator?.userAgent?.includes(`Cloudflare`))return!1;try{return Function(``),!0}catch{return!1}});function lt(e){if(st(e)===!1)return!1;let t=e.constructor;if(t===void 0||typeof t!=`function`)return!0;let n=t.prototype;return st(n)!==!1&&Object.prototype.hasOwnProperty.call(n,`isPrototypeOf`)!==!1}function ut(e){return lt(e)?{...e}:Array.isArray(e)?[...e]:e instanceof Map?new Map(e):e instanceof Set?new Set(e):e}var dt=new Set([`string`,`number`,`symbol`]);function ft(e){return e.replace(/[.*+?^${}()|[\]\\]/g,`\\$&`)}function L(e,t,n){let r=new e._zod.constr(t??e._zod.def);return(!t||n?.parent)&&(r._zod.parent=e),r}function R(e){let t=e;if(!t)return{};if(typeof t==`string`)return{error:()=>t};if(t?.message!==void 0){if(t?.error!==void 0)throw Error("Cannot specify both `message` and `error` params");t.error=t.message}return delete t.message,typeof t.error==`string`?{...t,error:()=>t.error}:t}function pt(e){return typeof e==`bigint`?e.toString()+`n`:typeof e==`string`?`"${e}"`:`${e}`}function mt(e){return Object.keys(e).filter(t=>e[t]._zod.optin!==void 0&&e[t]._zod.optout===`optional`)}var ht={safeint:[-(2**53-1),2**53-1],int32:[-2147483648,2147483647],uint32:[0,4294967295],float32:[-34028234663852886e22,34028234663852886e22],float64:[-Number.MAX_VALUE,Number.MAX_VALUE]},gt={int64:[BigInt(`-9223372036854775808`),BigInt(`9223372036854775807`)],uint64:[BigInt(0),BigInt(`18446744073709551615`)]};function _t(e,t){let n=e._zod.def,r=n.checks;if(r&&r.length>0)throw Error(`.pick() cannot be used on object schemas containing refinements`);let i={};return nt(i,e,vt(e,t)),L(e,I(n,{shape:i,checks:[]}))}function vt(e,t){let n=F(e),r=[];for(let e of Reflect.ownKeys(t)){if(!Object.getOwnPropertyDescriptor(n,e)?.enumerable)throw Error(`Unrecognized key: "${String(e)}"`);t[e]&&r.push(e)}return r}function yt(e,t){let n=e._zod.def,r=n.checks;if(r&&r.length>0)throw Error(`.omit() cannot be used on object schemas containing refinements`);let i=new Set(vt(e,t)),a={};return nt(a,e,Reflect.ownKeys(F(e)).filter(e=>!i.has(e))),L(e,I(n,{shape:a,checks:[]}))}function bt(e,t){if(!lt(t))throw Error(`Invalid input to extend: expected a plain object`);let n=e._zod.def.checks;if(n&&n.length>0){let n=F(e);for(let e of Reflect.ownKeys(t))if(Object.getOwnPropertyDescriptor(n,e)!==void 0)throw Error("Cannot overwrite keys on object schemas containing refinements. Use `.safeExtend()` instead.")}return L(e,I(e._zod.def,{shape:xt(e,t)}))}function xt(e,t){let n={};return nt(n,e,Reflect.ownKeys(F(e))),rt(n,t),n}function St(e,t){if(!lt(t))throw Error(`Invalid input to safeExtend: expected a plain object`);return L(e,I(e._zod.def,{shape:xt(e,t)}))}function Ct(e,t){if(!t?._zod?.def)throw Error("Invalid input to merge: expected an object schema. To merge a plain shape, use `.extend()`.");if(e._zod.def.checks?.length)throw Error(`.merge() cannot be used on object schemas containing refinements. Use .safeExtend() instead.`);let n={};return nt(n,e,Reflect.ownKeys(F(e))),nt(n,t,Reflect.ownKeys(F(t))),L(e,I(e._zod.def,{shape:n,get catchall(){return t._zod.def.catchall},checks:t._zod.def.checks??[]}))}function wt(e,t,n,r=`partial`){let i=t._zod.def.checks;if(i&&i.length>0)throw Error(`.${r}() cannot be used on object schemas containing refinements`);let a=n?new Set(vt(t,n)):void 0,o={};return nt(o,t,Reflect.ownKeys(F(t)),e&&((t,n)=>a&&!a.has(n)?t:new e({type:`optional`,innerType:t}))),L(t,I(t._zod.def,{shape:o,checks:[]}))}function Tt(e,t,n){let r=n?new Set(vt(t,n)):void 0,i={};return nt(i,t,Reflect.ownKeys(F(t)),(t,n)=>r&&!r.has(n)?t:new e({type:`nonoptional`,innerType:t})),L(t,I(t._zod.def,{shape:i}))}function z(e,t=0){if(e.aborted===!0)return!0;for(let n=t;n<e.issues.length;n++)if(e.issues[n]?.continue!==!0)return!0;return!1}function Et(e,t=0){if(e.aborted===!0)return!0;for(let n=t;n<e.issues.length;n++)if(e.issues[n]?.continue===!1)return!0;return!1}function Dt(e,t){return t.map(t=>{var n;return(n=t).path??(n.path=[]),t.path.unshift(e),t})}function Ot(e){return typeof e==`string`?e:e?.message}function kt(e){let t=[];for(let n of e??[]){let e=n._zod.def;if(!/_(length|size)$|_equals$/.test(e.check))break;let r=e.maximum??e.length??e.size;r>=0&&t.push([r,n])}return t}function At(e,t,n){var r;for(let i=t;i<e.length;i++)(r=e[i]).schema??(r.schema=n)}function jt(e,t,n){var r;let i=e.inst?._zod?.traits;i?.has(`$ZodType`)&&(i.has(`$ZodCheck`)?(r=e).schema??(r.schema=e.inst):e.schema=e.inst);let a=e.schema===e.inst?void 0:e.schema?._zod.def?.error,o=e.message?e.message:Ot(e.inst?._zod.def?.error?.(e))??Ot(a?.(e))??Ot(t?.error?.(e))??Ot(n.customError?.(e))??Ot(n.localeError?.(e))??`Invalid input`,s={};for(let t of Object.keys(e))t!==`inst`&&t!==`schema`&&t!==`continue`&&t!==`input`&&t!==`__proto__`&&(s[t]=e[t]);return s.path??=[],s.message=o,t?.reportInput&&(s.input=e.input),s}var Mt=/[\uD800-\uDBFF]/;function Nt(e){let t=e.length;if(!Mt.test(e))return t;let n=t;for(let r=0;r<t-1;r++)(e.charCodeAt(r)&64512)==55296&&(e.charCodeAt(r+1)&64512)==56320&&(n--,r++);return n}function Pt(e){return Array.isArray(e)?`array`:typeof e==`string`?`string`:`unknown`}function Ft(e){let t=typeof e;switch(t){case`number`:return Number.isNaN(e)?`nan`:`number`;case`object`:{if(e===null)return`null`;if(Array.isArray(e))return`array`;let t=e;if(t&&Object.getPrototypeOf(t)!==Object.prototype&&`constructor`in t&&t.constructor)return t.constructor.name}}return t}function It(...e){let[t,n,r]=e;return typeof t==`string`?{message:t,code:`custom`,input:n,inst:r}:{...t}}function Lt(e,t){for(let n in t){let r=Object.getOwnPropertyDescriptor(t,n);r.get?Object.defineProperty(e,n,{...r,enumerable:!1}):Bt(e,n,r.value)}}function B(e,t,n,r=!0){return Object.defineProperty(e,t,{configurable:!0,writable:!0,enumerable:r,value:n}),n}function Rt(e,t,n){return B(e,t,n,!1)}function zt(e,t){for(let n in e){let r=e[n];Object.defineProperty(t,n,{configurable:!0,enumerable:!0,get(){return B(this,n,r(this))},set(e){B(this,n,e)}})}return t}function Bt(e,t,n){Object.defineProperty(e,t,{configurable:!0,get(){return this==null?n:B(this,t,n.bind(this))},set(e){B(this,t,e)}})}function Vt(e,t){let n=Object.getPrototypeOf(e);return t in n?void 0:n}var Ht,Ut=!1,Wt={configurable:!0,get(){Ut=!0}};function V(e,t,n){let r=Object.getPrototypeOf(e._zod);t in r&&Ht!==e._zod?Ht=void 0:(Ht=e._zod,Object.defineProperty(r,t,{configurable:!0,get(){Object.defineProperty(this,t,Wt);let e=Ut;Ut=!1;try{let r=n(this);return Ut?delete this[t]:Object.defineProperty(this,t,{configurable:!0,writable:!0,value:r}),Ut||=e,r}catch(n){throw delete this[t],Ut||=e,n}},set(e){Object.defineProperty(this,t,{configurable:!0,writable:!0,value:e})}}))}function Gt(e,t,n,r){let i=Vt(e,t);i&&Object.defineProperty(i,t,{configurable:!0,get(){let e={configurable:!0,writable:!0,enumerable:r,value:void 0};return Object.defineProperty(this,t,e),e.value=n(this),Object.defineProperty(this,t,e),e.value},set(e){Object.defineProperty(this,t,{configurable:!0,writable:!0,enumerable:r,value:e})}})}var Kt=`~constantCatch`;function qt(e){let t=()=>e;return t[Kt]=!0,t}var Jt,Yt={value:void 0,enumerable:!1},Xt=`captureStackTrace`in Error?Error:null;function Zt(e){let t=Xt;if(t){let n=t.stackTraceLimit;if(typeof n==`number`){try{t.stackTraceLimit=0}catch{return Xt=null,new e}try{return new e}finally{t.stackTraceLimit=n}}}return new e}function H(e,t,n,r){let i={};function a(e){this.def=e,this.constr=d,this.traits=new Set}a.prototype=i;let o=n,s=o&&new WeakSet;function c(n,r){if(!n._zod){Yt.value=new a(r);try{Object.defineProperty(n,"_zod",Yt)}finally{Yt.value=void 0}}else if(n._zod.traits.has(e))return;if(n._zod.traits.add(e),t(n,r),s){let e=Object.getPrototypeOf(n),t=n._zod.constr.prototype,r=e;for(;r&&r!==t;)r=Object.getPrototypeOf(r);let i=r??e;s.has(i)||(s.add(i),Lt(i,o))}let i=d.prototype;for(let e in i)Object.prototype.hasOwnProperty.call(i,e)&&(e in n||(n[e]=i[e].bind(n)))}let l=r?.Parent??Object;class u extends l{}Object.defineProperty(u,"name",{value:e});function d(e){let t=r?.Parent?Zt(u):this;c(t,e);let n=t._zod.deferred;if(n){for(let e of n)e();t._zod.deferred=void 0}let i=globalThis.__zod_globalConfig?.postProcessor;return i&&i(t),t}return Object.defineProperty(d,"init",{value:c}),Object.defineProperty(d,Symbol.hasInstance,{value:t=>r?.Parent&&t instanceof r.Parent?!0:t?._zod?.traits?.has(e)}),Object.defineProperty(d,"name",{value:e}),d}var Qt=class extends Error{constructor(){super(`Encountered Promise during synchronous parse. Use .parseAsync() instead.`)}},$t=class extends Error{constructor(e){super(`Encountered unidirectional transform during encode: ${e}`),this.name=`ZodEncodeError`}};(Jt=globalThis).__zod_globalConfig??(Jt.__zod_globalConfig={});var U=globalThis.__zod_globalConfig;function W(e){return e&&Object.assign(U,e),U}function en(){let e=this._zod;return e.message??=JSON.stringify(e.def,Ke,2),e.message}function tn(e){this._zod.message=e}var nn={get:en,set:tn,enumerable:!0,configurable:!0},rn={value:void 0,enumerable:!1},an=new WeakSet([Object.prototype,Error.prototype]),on=H(`$ZodError`,(e,t)=>{e.name=`$ZodError`,rn.value=t,Object.defineProperty(e,"issues",rn),rn.value=void 0,Object.defineProperty(e,"message",nn);let n=Object.getPrototypeOf(e);an.has(n)||(an.add(n),Object.defineProperty(n,"toString",{configurable:!0,enumerable:!1,get(){let e=()=>this.message;return Object.defineProperty(this,"toString",{value:e,configurable:!0,writable:!0}),e},set(e){Object.defineProperty(this,"toString",{value:e,configurable:!0,writable:!0})}}))});function sn(e,t,n){return Object.prototype.hasOwnProperty.call(e,t)||(t===`__proto__`?Object.defineProperty(e,t,{value:n(),writable:!0,enumerable:!0,configurable:!0}):e[t]=n()),e[t]}function cn(e,t=e=>e.message){let n={},r=[];for(let i of e.issues)i.path.length>0?sn(n,i.path[0],()=>[]).push(t(i)):r.push(t(i));return{formErrors:r,fieldErrors:n}}function ln(e,t=e=>e.message){let n={_errors:[]},r=(e,i=[])=>{for(let a of e.issues)if(a.code===`invalid_union`&&a.errors.length)a.errors.map(e=>r({issues:e},[...i,...a.path]));else if(a.code===`invalid_key`)r({issues:a.issues},[...i,...a.path]);else if(a.code===`invalid_element`)r({issues:a.issues},[...i,...a.path]);else{let e=[...i,...a.path];if(e.length===0)n._errors.push(t(a));else{let r=n,i=0;for(;i<e.length;){let n=e[i],o=i===e.length-1;if(n===`_errors`){o&&r._errors.push(t(a)),i++;continue}Object.prototype.hasOwnProperty.call(r,n)||Object.defineProperty(r,n,{value:{_errors:[]},enumerable:!0,writable:!0,configurable:!0});let s=r[n];o&&s._errors.push(t(a)),r=s,i++}}}};return r(e),n}function un(e,t){return{callee:t?.callee??e,Err:t?.Err}}var dn=e=>{let t=(n,r,i,a)=>{let o=i?{...i,async:!1}:{async:!1},s=n._zod.run({value:r,issues:[]},o);if(s instanceof Promise)throw new Qt;if(s.issues.length){let n=new((a?.Err)??e)(s.issues.map(e=>jt(e,o,W())));throw ot(n,a?.callee??t),n}return s.value};return t},fn=e=>{let t=async(n,r,i,a)=>{let o=i?{...i,async:!0}:{async:!0},s=n._zod.run({value:r,issues:[]},o);if(s instanceof Promise&&(s=await s),s.issues.length){let n=new((a?.Err)??e)(s.issues.map(e=>jt(e,o,W())));throw ot(n,a?.callee??t),n}return s.value};return t},pn=e=>(t,n,r)=>{let i=r?{...r,async:!1}:{async:!1},a=t._zod.run({value:n,issues:[]},i);if(a instanceof Promise)throw new Qt;return a.issues.length?mn(e,a.issues,i):{success:!0,data:a.value}};function mn(e,t,n){let r;return{success:!1,get error(){return r||(r=new e(t.map(e=>jt(e,n,W()))),t=void 0,n=void 0),r},set error(e){r=e,t=void 0,n=void 0}}}var hn=e=>async(t,n,r)=>{let i=r?{...r,async:!0}:{async:!0},a=t._zod.run({value:n,issues:[]},i);return a instanceof Promise&&(a=await a),a.issues.length?mn(e,a.issues,i):{success:!0,data:a.value}},gn=Symbol.for(`zod.compile.invalid`),_n=Symbol.for(`zod.compile.fallback`),vn=((e,t,n)=>{let r=e._zod.bag.validator;if(r!==void 0){if(r(t)!==gn)return!0;if(r.definite===!0&&n===void 0)return!1}return yn(e,t,n)});function yn(e,t,n){let r=n?{...n,async:!1,abortEarly:!0}:{async:!1,abortEarly:!0},i=e._zod.bag.fallbackRun,a;if(i?(r[_n]=!0,a=i({value:t,issues:[]},r)):a=e._zod.run({value:t,issues:[]},r),a instanceof Promise)throw new Qt;return a.issues.length===0}var bn=async(e,t,n)=>{let r=n?{...n,async:!0,abortEarly:!0}:{async:!0,abortEarly:!0},i=e._zod.run({value:t,issues:[]},r);return i instanceof Promise&&(i=await i),i.issues.length===0},xn=e=>{let t=dn(e),n=(e,r,i,a)=>{let o=i?{...i,direction:`backward`}:{direction:`backward`};return t(e,r,o,un(n,a))};return n},Sn=e=>{let t=dn(e),n=(e,r,i,a)=>t(e,r,i,un(n,a));return n},Cn=e=>{let t=fn(e),n=async(e,r,i,a)=>{let o=i?{...i,direction:`backward`}:{direction:`backward`};return await t(e,r,o,un(n,a))};return n},wn=e=>{let t=fn(e),n=async(e,r,i,a)=>await t(e,r,i,un(n,a));return n},Tn=e=>(t,n,r)=>{let i=r?{...r,direction:`backward`}:{direction:`backward`};return pn(e)(t,n,i)},En=e=>(t,n,r)=>pn(e)(t,n,r),Dn=e=>async(t,n,r)=>{let i=r?{...r,direction:`backward`}:{direction:`backward`};return hn(e)(t,n,i)},On=e=>async(t,n,r)=>hn(e)(t,n,r),kn=/^[cC][0-9a-z]{6,}$/,An=/^[0-9a-z]+$/,jn=/^[0-7][0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{25}$/,Mn=/^[0-9a-vA-V]{20}$/,Nn=/^[A-Za-z0-9]{27}$/,Pn=/^[a-zA-Z0-9_-]{21}$/;function Fn(e){return RegExp(`^[a-zA-Z0-9_-]{${e}}$`)}var In=/^P(?:(\d+W)|(?!.*W)(?=\d|T\d)(\d+Y)?(\d+M)?(\d+D)?(T(?=\d)(\d+H)?(\d+M)?(\d+([.,]\d+)?S)?)?)$/,Ln=/^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$/,Rn=e=>e?RegExp(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-${e}[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12})$`):/^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$/,zn=/^(?:[A-Za-z0-9_'+\-]+\.)*[A-Za-z0-9_'+\-]*[A-Za-z0-9_+-]@(?:[A-Za-z0-9][A-Za-z0-9\-]*\.)+[A-Za-z]{2,}$/,Bn=`^(?=[\\s\\S]*[\\p{Extended_Pictographic}\\p{Regional_Indicator}\\u20E3])[\\p{Extended_Pictographic}\\p{Emoji_Component}]+$`;function Vn(){return new RegExp(Bn,`u`)}var Hn=/^(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])$/,Un=/^(([0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,7}:|([0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,5}(:[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}:){1,4}(:[0-9a-fA-F]{1,4}){1,3}|([0-9a-fA-F]{1,4}:){1,3}(:[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}:){1,2}(:[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:((:[0-9a-fA-F]{1,4}){1,6})|:((:[0-9a-fA-F]{1,4}){1,7}|:))$/,Wn=/^((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\/([0-9]|[1-2][0-9]|3[0-2])$/,Gn=/^(([0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,7}:|([0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,5}(:[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}:){1,4}(:[0-9a-fA-F]{1,4}){1,3}|([0-9a-fA-F]{1,4}:){1,3}(:[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}:){1,2}(:[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:((:[0-9a-fA-F]{1,4}){1,6})|:((:[0-9a-fA-F]{1,4}){1,7}|:))\/(12[0-8]|1[01][0-9]|[1-9]?[0-9])$/,Kn=/^$|^(?:[0-9a-zA-Z+/]{4})*(?:(?:[0-9a-zA-Z+/]{2}==)|(?:[0-9a-zA-Z+/]{3}=))?$/,qn=/^(?:[A-Za-z0-9_-]{4})*(?:[A-Za-z0-9_-]{2,3})?$/,Jn=/^https?$/,Yn=/^\+[1-9]\d{6,14}$/,Xn=`(?:(?:\\d\\d[2468][048]|\\d\\d[13579][26]|\\d\\d0[48]|[02468][048]00|[13579][26]00)-02-29|\\d{4}-(?:(?:0[13578]|1[02])-(?:0[1-9]|[12]\\d|3[01])|(?:0[469]|11)-(?:0[1-9]|[12]\\d|30)|(?:02)-(?:0[1-9]|1\\d|2[0-8])))`;function Zn(e){return RegExp(`^${e}$`)}var Qn=Zn(Xn);function $n(e){let t=`(?:[01]\\d|2[0-3]):[0-5]\\d`;return typeof e.precision==`number`?e.precision===-1?`${t}`:e.precision===0?`${t}:[0-5]\\d`:`${t}:[0-5]\\d\\.\\d{${e.precision}}`:e.seconds?`${t}:[0-5]\\d(?:\\.\\d+)?`:`${t}(?::[0-5]\\d(?:\\.\\d+)?)?`}function er(e){return RegExp(`^${$n(e)}$`)}function tr(e){let t=[`Z`];e.offset&&t.push(`([+-](?:[01]\\d|2[0-3]):[0-5]\\d)`);let n=`${$n({precision:e.precision,seconds:!0})}(?:${t.join(`|`)})`,r=e.local?`${n}|${$n({precision:e.precision})}`:n;return RegExp(`^${Xn}T(?:${r})$`)}var nr=/^[\s\S]{0,}$/,rr=/^-?\d+(?:\.\d+)?$/,ir=/^[^A-Z]*$/,ar=/^[^a-z]*$/,G=H(`$ZodCheck`,(e,t)=>{var n;e._zod??={},e._zod.def=t,(n=e._zod).onattach??(n.onattach=[])}),or=e=>{let t=e.value;return!Ye(t)&&t.length!==void 0},sr={number:`number`,bigint:`bigint`,object:`date`},cr=H(`$ZodCheckLessThan`,(e,t)=>{G.init(e,t);let n=sr[typeof t.value];e._zod.check=r=>{(t.inclusive?r.value<=t.value:r.value<t.value)||r.issues.push({origin:sr[typeof r.value]??n,code:`too_big`,maximum:typeof t.value==`object`?t.value.getTime():t.value,input:r.value,inclusive:t.inclusive,inst:e,continue:!t.abort})}}),lr=H(`$ZodCheckGreaterThan`,(e,t)=>{G.init(e,t);let n=sr[typeof t.value];e._zod.check=r=>{(t.inclusive?r.value>=t.value:r.value>t.value)||r.issues.push({origin:sr[typeof r.value]??n,code:`too_small`,minimum:typeof t.value==`object`?t.value.getTime():t.value,input:r.value,inclusive:t.inclusive,inst:e,continue:!t.abort})}}),ur=H(`$ZodCheckMultipleOf`,(e,t)=>{G.init(e,t),e._zod.check=n=>{if(typeof n.value!=typeof t.value)throw Error(`Cannot mix number and bigint in multiple_of check.`);(typeof n.value==`bigint`?t.value!==BigInt(0)&&n.value%t.value===BigInt(0):Ze(n.value,t.value)===0)||n.issues.push({origin:typeof n.value,code:`not_multiple_of`,divisor:t.value,input:n.value,inst:e,continue:!t.abort})}}),dr=H(`$ZodCheckNumberFormat`,(e,t)=>{G.init(e,t),t.format=t.format||`float64`;let n=t.format?.includes(`int`),r=n?`int`:`number`,[i,a]=ht[t.format];e._zod.check=o=>{let s=o.value;if(n){if(!Number.isInteger(s)){o.issues.push({expected:r,format:t.format,code:`invalid_type`,continue:!1,input:s,inst:e});return}if(!Number.isSafeInteger(s)){s>0?o.issues.push({input:s,code:`too_big`,maximum:2**53-1,note:`Integers must be within the safe integer range.`,inst:e,origin:r,inclusive:!0,continue:!t.abort}):o.issues.push({input:s,code:`too_small`,minimum:-(2**53-1),note:`Integers must be within the safe integer range.`,inst:e,origin:r,inclusive:!0,continue:!t.abort});return}}s<i&&o.issues.push({origin:`number`,input:s,code:`too_small`,minimum:i,inclusive:!0,inst:e,continue:!t.abort}),s>a&&o.issues.push({origin:`number`,input:s,code:`too_big`,maximum:a,inclusive:!0,inst:e,continue:!t.abort})}}),fr=H(`$ZodCheckMaxLength`,(e,t)=>{var n;G.init(e,t),(n=e._zod.def).when??(n.when=or),e._zod.check=n=>{let r=n.value,i=r.length;if((typeof r==`string`&&i>t.maximum?Nt(r):i)<=t.maximum)return;let a=Pt(r);n.issues.push({origin:a,code:`too_big`,maximum:t.maximum,inclusive:!0,input:r,inst:e,continue:!t.abort})}}),pr=H(`$ZodCheckMinLength`,(e,t)=>{var n;G.init(e,t),(n=e._zod.def).when??(n.when=or),e._zod.check=n=>{let r=n.value,i=r.length;if((typeof r==`string`&&i>=t.minimum&&i<t.minimum*2?Nt(r):i)>=t.minimum)return;let a=Pt(r);n.issues.push({origin:a,code:`too_small`,minimum:t.minimum,inclusive:!0,input:r,inst:e,continue:!t.abort})}}),mr=H(`$ZodCheckLengthEquals`,(e,t)=>{var n;G.init(e,t),(n=e._zod.def).when??(n.when=or),e._zod.check=n=>{let r=n.value,i=r.length,a=typeof r==`string`&&i>=t.length&&i<=t.length*2?Nt(r):i;if(a===t.length)return;let o=Pt(r),s=a>t.length;n.issues.push({origin:o,...s?{code:`too_big`,maximum:t.length}:{code:`too_small`,minimum:t.length},inclusive:!0,exact:!0,input:n.value,inst:e,continue:!t.abort})}}),hr=H(`$ZodCheckStringFormat`,(e,t)=>{var n,r;G.init(e,t),t.pattern?(n=e._zod).check??(n.check=n=>{t.pattern.lastIndex=0,!t.pattern.test(n.value)&&n.issues.push({origin:`string`,code:`invalid_format`,format:t.format,input:n.value,...t.pattern?{pattern:t.pattern.toString()}:{},inst:e,continue:!t.abort})}):(r=e._zod).check??(r.check=()=>{})}),gr=H(`$ZodCheckRegex`,(e,t)=>{hr.init(e,t),e._zod.check=n=>{t.pattern.lastIndex=0,!t.pattern.test(n.value)&&n.issues.push({origin:`string`,code:`invalid_format`,format:`regex`,input:n.value,pattern:t.pattern.toString(),inst:e,continue:!t.abort})}}),_r=H(`$ZodCheckLowerCase`,(e,t)=>{t.pattern??=ir,hr.init(e,t)}),vr=H(`$ZodCheckUpperCase`,(e,t)=>{t.pattern??=ar,hr.init(e,t)}),yr=H(`$ZodCheckIncludes`,(e,t)=>{G.init(e,t);let n=ft(t.includes);t.pattern=new RegExp(typeof t.position==`number`?`^.{${t.position},}${n}`:n),e._zod.check=n=>{n.value.includes(t.includes,t.position)||n.issues.push({origin:`string`,code:`invalid_format`,format:`includes`,includes:t.includes,input:n.value,inst:e,continue:!t.abort})}}),br=H(`$ZodCheckStartsWith`,(e,t)=>{G.init(e,t);let n=RegExp(`^${ft(t.prefix)}.*`);t.pattern??=n,e._zod.check=n=>{n.value.startsWith(t.prefix)||n.issues.push({origin:`string`,code:`invalid_format`,format:`starts_with`,prefix:t.prefix,input:n.value,inst:e,continue:!t.abort})}}),xr=H(`$ZodCheckEndsWith`,(e,t)=>{G.init(e,t);let n=RegExp(`.*${ft(t.suffix)}$`);t.pattern??=n,e._zod.check=n=>{n.value.endsWith(t.suffix)||n.issues.push({origin:`string`,code:`invalid_format`,format:`ends_with`,suffix:t.suffix,input:n.value,inst:e,continue:!t.abort})}}),Sr=H(`$ZodCheckOverwrite`,(e,t)=>{G.init(e,t),e._zod.check=e=>{e.value=t.tx(e.value)}}),Cr=class{constructor(e=[],t={}){this.content=[],this.indent=0,this.args=e,this.closed=t}indented(e){this.indent+=1;try{e(this)}finally{--this.indent}}write(e){if(typeof e==`function`){e(this,{execution:`sync`}),e(this,{execution:`async`});return}let t=e.split(`
`).filter(e=>e),n=Math.min(...t.map(e=>e.length-e.trimStart().length)),r=t.map(e=>e.slice(n)).map(e=>` `.repeat(this.indent*2)+e);for(let e of r)this.content.push(e)}compile(){let e=Function,t=this?.content??[``];return new e(...Object.keys(this.closed),`return function (${this.args.join(`, `)}) {\n${t.join(`
`)}\n};`)(...Object.values(this.closed))}},wr={major:4,minor:6,patch:7},K=H(`$ZodType`,(e,t)=>{var n;e??={},e._zod.def=t,e._zod.bag=e._zod.bag||{},e._zod.version=wr;let r=e._zod.def.checks,i=e._zod.traits.has(`$ZodCheck`)?[e,...r??[]]:r?.length?[...r]:[];for(let t of i)for(let n of t._zod.onattach)n(e);if(i.length===0)(n=e._zod).deferred??(n.deferred=[]),e._zod.deferred?.push(()=>{e._zod.run=e._zod.parse});else{let t=(t,n,r)=>{if(t.memo)return t;let i=z(t),a;for(let o of n){if(o._zod.def.when){if(Et(t)||!o._zod.def.when(t))continue}else if(i)continue;let n=t.issues.length,s=o._zod.check(t);if(s instanceof Promise&&r?.async===!1)throw new Qt;if(a||s instanceof Promise)a=(a??Promise.resolve()).then(async()=>{await s,t.issues.length!==n&&(At(t.issues,n,e),i||=z(t,n))});else{if(t.issues.length===n)continue;At(t.issues,n,e),i||=z(t,n)}}return a?a.then(()=>t):t},n=(n,r,a)=>{if(z(n))return n.aborted=!0,n;let o=t(r,i,a);if(o instanceof Promise){if(a.async===!1)throw new Qt;return o.then(t=>e._zod.parse(t,a))}return e._zod.parse(o,a)};e._zod.run=(r,a)=>{if(a.skipChecks)return e._zod.parse(r,a);if(a.direction===`backward`){let t=e._zod.parse({value:r.value,issues:[]},{...a,skipChecks:!0});return t instanceof Promise?t.then(e=>n(e,r,a)):n(t,r,a)}let o=e._zod.parse(r,a);if(o instanceof Promise){if(a.async===!1)throw new Qt;return o.then(e=>t(e,i,a))}return t(o,i,a)}}},{get"~standard"(){return Rt(this,`~standard`,Dr(this))},set"~standard"(e){B(this,`~standard`,e)}}),Tr=(e,t)=>e.issues.length?{issues:e.issues.map(e=>jt(e,t,W()))}:{value:e.value};async function Er(e,t){let n={async:!0};return Tr(await e._zod.run({value:t,issues:[]},n),n)}function Dr(e){return{validate:t=>{let n={async:!1};try{let r=e._zod.run({value:t,issues:[]},n);if(!(r instanceof Promise))return Tr(r,n)}catch{}return Er(e,t)},vendor:`zod`,version:1}}var Or=H(`$ZodString`,(e,t)=>{K.init(e,t),e._zod.pattern=t.pattern??nr,e._zod.parse=(n,r)=>{if(t.coerce)try{n.value=String(n.value)}catch{}return typeof n.value==`string`||n.issues.push({expected:`string`,code:`invalid_type`,input:n.value,inst:e}),n}}),q=H(`$ZodStringFormat`,(e,t)=>{hr.init(e,t),Or.init(e,t)}),kr=H(`$ZodGUID`,(e,t)=>{t.pattern??=Ln,q.init(e,t)}),Ar=H(`$ZodUUID`,(e,t)=>{if(t.version){let e={v1:1,v2:2,v3:3,v4:4,v5:5,v6:6,v7:7,v8:8}[t.version];if(e===void 0)throw Error(`Invalid UUID version: "${t.version}"`);t.pattern??=Rn(e)}else t.pattern??=Rn();q.init(e,t)}),jr=H(`$ZodEmail`,(e,t)=>{t.pattern??=zn,q.init(e,t)});function Mr(e){try{return typeof URL<`u`&&typeof URL.canParse==`function`?URL.canParse(e):(new URL(e),!0)}catch{return!1}}function Nr(e,t){return!(`normalize`in t)&&!(`hostname`in t)&&!(`protocol`in t)?Mr(e)||2:Pr(e,t)}function Pr(e,t){if(!t.normalize&&t.protocol?.source===Jn.source&&!/^https?:\/\//i.test(e))return 1;try{if(typeof URL<`u`){let t=URL;if(typeof t.parse==`function`)return t.parse(e)??2}return new URL(e)}catch{return 2}}var Fr=/[\t\n\r]/g;function Ir(e){return e.replace(Fr,``)}function Lr(e,t){return t.lastIndex=0,t.test(e.hostname)}function Rr(e,t){return t.lastIndex=0,t.test(e.protocol.endsWith(`:`)?e.protocol.slice(0,-1):e.protocol)}var zr=H(`$ZodURL`,(e,t)=>{q.init(e,t),e._zod.check=n=>{try{let r=n.value.trim(),i=Nr(r,t);if(i===1){n.issues.push({code:`invalid_format`,format:`url`,note:`Invalid URL format`,input:n.value,inst:e,continue:!t.abort});return}if(i===2){n.issues.push({code:`invalid_format`,format:`url`,input:n.value,inst:e,continue:!t.abort});return}if(i===!0){n.value=Ir(r);return}t.hostname&&!Lr(i,t.hostname)&&n.issues.push({code:`invalid_format`,format:`url`,note:`Invalid hostname`,pattern:t.hostname.source,input:n.value,inst:e,continue:!t.abort}),t.protocol&&!Rr(i,t.protocol)&&n.issues.push({code:`invalid_format`,format:`url`,note:`Invalid protocol`,pattern:t.protocol.source,input:n.value,inst:e,continue:!t.abort}),n.value=t.normalize?i.href:Ir(r);return}catch{n.issues.push({code:`invalid_format`,format:`url`,input:n.value,inst:e,continue:!t.abort})}}}),Br=H(`$ZodEmoji`,(e,t)=>{t.pattern??=Vn(),q.init(e,t)}),Vr=H(`$ZodNanoID`,(e,t)=>{if(t.length!==void 0&&(!Number.isInteger(t.length)||t.length<1))throw Error(`Invalid nanoid length: ${t.length}`);t.pattern??=t.length===void 0?Pn:Fn(t.length),q.init(e,t)}),Hr=H(`$ZodCUID`,(e,t)=>{t.pattern??=kn,q.init(e,t)}),Ur=H(`$ZodCUID2`,(e,t)=>{t.pattern??=An,q.init(e,t)}),Wr=H(`$ZodULID`,(e,t)=>{t.pattern??=jn,q.init(e,t)}),Gr=H(`$ZodXID`,(e,t)=>{t.pattern??=Mn,q.init(e,t)}),Kr=H(`$ZodKSUID`,(e,t)=>{t.pattern??=Nn,q.init(e,t)}),qr=H(`$ZodISODateTime`,(e,t)=>{t.pattern??=tr(t),q.init(e,t)}),Jr=H(`$ZodISODate`,(e,t)=>{t.pattern??=Qn,q.init(e,t)}),Yr=H(`$ZodISOTime`,(e,t)=>{t.pattern??=er(t),q.init(e,t)}),Xr=H(`$ZodISODuration`,(e,t)=>{t.pattern??=In,q.init(e,t)}),Zr=H(`$ZodIPv4`,(e,t)=>{t.pattern??=Hn,q.init(e,t)}),Qr=/^[0-9a-fA-F:.]+$/;function $r(e){return Qr.test(e)?Mr(`http://[${e}]`):!1}var ei=H(`$ZodIPv6`,(e,t)=>{t.pattern??=Un,q.init(e,t),e._zod.check=n=>{$r(n.value)||n.issues.push({code:`invalid_format`,format:`ipv6`,input:n.value,inst:e,continue:!t.abort})}}),ti=H(`$ZodCIDRv4`,(e,t)=>{t.pattern??=Wn,q.init(e,t)});function ni(e){let t=e.split(`/`);if(t.length!==2)return!1;let[n,r]=t;if(!r)return!1;let i=Number(r);return`${i}`!==r||i<0||i>128?!1:$r(n)}var ri=H(`$ZodCIDRv6`,(e,t)=>{t.pattern??=Gn,q.init(e,t),e._zod.check=n=>{ni(n.value)||n.issues.push({code:`invalid_format`,format:`cidrv6`,input:n.value,inst:e,continue:!t.abort})}});function ii(e){if(e===``)return!0;if(/\s/.test(e)||e.length%4!=0)return!1;try{return atob(e),!0}catch{return!1}}var ai=/^[0-9a-zA-Z+/]*={0,2}$/,oi=H(`$ZodBase64`,(e,t)=>{t.pattern??=ai,q.init(e,t),e._zod.check=n=>{ii(n.value)||n.issues.push({code:`invalid_format`,format:`base64`,input:n.value,inst:e,continue:!t.abort})}}),si=/^[A-Za-z0-9_-]*$/;function ci(e){if(!si.test(e))return!1;let t=e.replace(/[-_]/g,e=>e===`-`?`+`:`/`);return ii(t.padEnd(Math.ceil(t.length/4)*4,`=`))}var li=H(`$ZodBase64URL`,(e,t)=>{t.pattern??=si,q.init(e,t),e._zod.check=n=>{ci(n.value)||n.issues.push({code:`invalid_format`,format:`base64url`,input:n.value,inst:e,continue:!t.abort})}}),ui=H(`$ZodE164`,(e,t)=>{t.pattern??=Yn,q.init(e,t)});function di(e,t=null){try{let n=e.split(`.`);if(n.length!==3)return!1;let[r]=n;if(!r)return!1;let i=JSON.parse(atob(r));return!(`typ`in i&&i?.typ!==`JWT`||!i.alg||t&&(!(`alg`in i)||i.alg!==t))}catch{return!1}}var fi=H(`$ZodJWT`,(e,t)=>{q.init(e,t),e._zod.check=n=>{di(n.value,t.alg)||n.issues.push({code:`invalid_format`,format:`jwt`,input:n.value,inst:e,continue:!t.abort})}}),pi=H(`$ZodNumber`,(e,t)=>{K.init(e,t),e._zod.pattern=rr,e._zod.parse=(n,r)=>{if(t.coerce)try{n.value=Number(n.value)}catch{}let i=n.value;if(typeof i==`number`&&!Number.isNaN(i)&&Number.isFinite(i))return n;let a=typeof i==`number`?Number.isNaN(i)?`NaN`:Number.isFinite(i)?void 0:String(i):void 0;return n.issues.push({expected:`number`,code:`invalid_type`,input:i,inst:e,...a?{received:a}:{}}),n}}),mi=H(`$ZodNumberFormat`,(e,t)=>{dr.init(e,t),pi.init(e,t)}),hi=H(`$ZodUnknown`,(e,t)=>{K.init(e,t),e._zod.parse=e=>e}),gi=H(`$ZodNever`,(e,t)=>{K.init(e,t),e._zod.parse=(t,n)=>(t.issues.push({expected:`never`,code:`invalid_type`,input:t.value,inst:e}),t)});function _i(e,t){let n=kt(e._zod.def.checks);if(!n.length)return;let r=Math.min(...n.map(e=>e[0])),i=e._zod.parse;e._zod.parse=(n,a)=>{let o=t(n.value);if(o>r&&!a?.skipChecks&&a?.direction!==`backward`){for(let[t,r]of kt(e._zod.def.checks))if(o>t&&r._zod.def.when(n)){let t=n.issues.length;return r._zod.check(n),n.issues[t].continue=!1,At(n.issues,t,e),n}}return i(n,a)}}var vi=e=>Array.isArray(e)?e.length:-1;function yi(e,t,n){e.issues.length&&t.issues.push(...Dt(n,e.issues)),t.value[n]=e.value}var bi=H(`$ZodArray`,(e,t)=>{K.init(e,t);let n=U.memoizer;n?.attach(e),e._zod.parse=(r,i)=>{let a=r.value;if(!Array.isArray(a))return r.issues.push({expected:`array`,code:`invalid_type`,input:a,inst:e}),r;r.value=n?n.alloc(e,r,Array(a.length),i):Array(a.length);let o=[],s=i?.abortEarly;for(let e=0;e<a.length;e++){let n=a[e],c=t.element._zod.run({value:n,issues:[]},i);if(c instanceof Promise)o.push(c.then(t=>yi(t,r,e)));else if(yi(c,r,e),s&&c.issues.length!==0&&z(c))break}return o.length?Promise.all(o).then(()=>r):r},_i(e,vi)});function xi(e,t,n,r,i,a){let o=n in r,s=a===`optional`;if(o||!s||i!==`optional`){if(e.issues.length){if(i!==void 0&&s&&!o)return;t.issues.push(...Dt(n,e.issues))}!o&&i===void 0?e.issues.length||t.issues.push({code:`invalid_type`,expected:`nonoptional`,input:void 0,path:[n]}):e.value===void 0?(o||i===`defaulted`&&!s)&&(t.value[n]=void 0):t.value[n]=e.value}}var Si=[];function Ci(e){let t=Object.keys(e.shape),n=Object.getOwnPropertySymbols(e.shape),r=n.length?n:Si,i=r.length?[...t,...r]:t;for(let t of i)if(!e.shape?.[t]?._zod?.traits?.has(`$ZodType`))throw Error(`Invalid element at key "${String(t)}": expected a Zod schema`);let a=mt(e.shape);return{...e,allKeys:i,symbolKeys:r,keySet:new Set(t),numKeys:t.length,optionalKeys:new Set(a)}}function wi(e,t,n,r,i,a,o){let s=[],c=i.keySet,l=i.catchall._zod,u=l.def.type,d=l.optin,f=l.optout,p=0;for(let i in t){if(o&&n.issues.length!==p){if(z(n,p))break;p=n.issues.length}if(c.has(i))continue;if(i===`__proto__`){u===`never`&&s.push(i);continue}if(u===`never`){s.push(i);continue}let a=l.run({value:t[i],issues:[]},r);a instanceof Promise?e.push(a.then(e=>xi(e,n,i,t,d,f))):xi(a,n,i,t,d,f)}return s.length&&n.issues.push({code:`unrecognized_keys`,keys:s,input:t,inst:a,continue:!0}),e.length?Promise.all(e).then(()=>n):n}var Ti=H(`$ZodObject`,(e,t)=>{K.init(e,t);let n=Object.getOwnPropertyDescriptor(t,`shape`),r=n?.get?n.get.raw:t.shape??{};if(r){let e=()=>{let n={...r};return Object.defineProperty(t,"shape",{value:n}),e.raw=n,n};e.raw=r,Object.defineProperty(t,"shape",{get:e})}let i=Je(()=>Ci(t));V(e,`propValues`,e=>{let t=e.def.shape,n={};for(let e in t){let r=t[e]._zod;if(r.values){Object.prototype.hasOwnProperty.call(n,e)||Qe(n,e,new Set);for(let t of r.values)n[e].add(t);r.optin!==void 0&&n[e].add(void 0)}}return n});let a=st,o=t.catchall,s,c=U.memoizer;c?.attach(e),e._zod.parse=(t,n)=>{s??=i.value;let r=t.value;if(!a(r))return t.issues.push({expected:`object`,code:`invalid_type`,input:r,inst:e}),t;t.value=c?c.alloc(e,t,{},n):{};let l=[],u=s.shape,d=n?.abortEarly,f=t.issues.length;for(let e of s.allKeys){if(d&&t.issues.length!==f){if(z(t,f))break;f=t.issues.length}if(e===`__proto__`)continue;let i=u[e],a=i._zod.optin,o=i._zod.optout,s=i._zod.run({value:r[e],issues:[]},n);s instanceof Promise?l.push(s.then(n=>xi(n,t,e,r,a,o))):xi(s,t,e,r,a,o)}return o?wi(l,r,t,n,i.value,e,d===!0):l.length?Promise.all(l).then(()=>t):t}}),Ei=H(`$ZodObjectJIT`,(e,t)=>{Ti.init(e,t);let n=e._zod.parse,r=Je(()=>Ci(t)),i=U.memoizer,a=t=>{let n=r.value,a=n.symbolKeys,o=new Cr([`payload`,`ctx`],{shape:t,inst:e,memo:i,syms:a}),s=e=>`shape[${e}]._zod.run({ value: input[${e}], issues: [] }, ctx)`,c=(e,t)=>`
          let ${e}_ab = false;
          for (let i = 0; i < ${e}.issues.length; i++) {
            const iss = ${e}.issues[i];
            iss.path = iss.path ? [${t}, ...iss.path] : [${t}];
            payload.issues.push(iss);
            if (iss.continue !== true) ${e}_ab = true;
          }
          if (${e}_ab && ctx && ctx.abortEarly) {
            payload.value = newResult;
            return payload;
          }`;o.write(`const input = payload.value;`);let l=Object.create(null),u=0;for(let e of n.allKeys)l[e]=`key_${u++}`;o.write(i?`const newResult = memo.alloc(inst, payload, {}, ctx);`:`const newResult = {};`);for(let e of n.allKeys){if(e===`__proto__`)continue;let n=l[e],r=typeof e==`symbol`?`syms[${a.indexOf(e)}]`:it(e),i=`${r} in input`,u=t[e],d=u?._zod?.optin,f=d!==void 0,p=u?._zod?.optout===`optional`;if(o.write(`const ${n} = ${s(r)};`),f&&p){let e=d===`optional`?`${n}_present`:`${n}.value !== undefined || ${n}_present`;o.write(`
        const ${n}_present = ${i};
        if (!${n}.issues.length || ${n}_present) {
          if (${n}.issues.length) {${c(n,r)}
          }

          if (${e}) {
            newResult[${r}] = ${n}.value;
          }
        }

      `)}else f?(o.write(`
        if (${n}.issues.length) {${c(n,r)}
        }
      `),d===`defaulted`?o.write(`newResult[${r}] = ${n}.value;`):o.write(`
        if (${n}.value !== undefined || ${i}) {
          newResult[${r}] = ${n}.value;
        }
      `)):o.write(`
        const ${n}_present = ${i};
        if (${n}.issues.length) {${c(n,r)}
        }
        if (!${n}_present && !${n}.issues.length) {
          payload.issues.push({
            code: "invalid_type",
            expected: "nonoptional",
            input: undefined,
            path: [${r}]
          });
          if (ctx && ctx.abortEarly) {
            payload.value = newResult;
            return payload;
          }
        }

        if (${n}_present) {
          newResult[${r}] = ${n}.value;
        }

      `)}return o.write(`payload.value = newResult;`),o.write(`return payload;`),o.compile()},o,s=st,c=!U.jitless,l=c&&ct.value,u=t.catchall,d;e._zod.parse=(i,f)=>{d??=r.value;let p=i.value;return s(p)?c&&l&&f?.async===!1&&f.jitless!==!0?(o||=a(t.shape),i=o(i,f),u?wi([],p,i,f,d,e,f?.abortEarly===!0):i):n(i,f):(i.issues.push({expected:`object`,code:`invalid_type`,input:p,inst:e}),i)}});function Di(e,t,n,r){for(let n of e)if(n.issues.length===0)return t.value=n.value,t;let i=e.filter(e=>!z(e));return i.length===1?(t.value=i[0].value,i[0]):(t.issues.push({code:`invalid_union`,input:t.value,inst:n,errors:e.map(e=>e.issues.map(e=>jt(e,r,W())))}),t)}var Oi=H(`$ZodUnion`,(e,t)=>{K.init(e,t),V(e,`optin`,e=>e.def.options.some(e=>e._zod.optin===`defaulted`)?`defaulted`:e.def.options.some(e=>e._zod.optin!==void 0)?`optional`:void 0),V(e,`optout`,e=>e.def.options.some(e=>e._zod.optout===`optional`)?`optional`:void 0),V(e,`values`,e=>{if(e.def.options.every(e=>e._zod.values))return new Set(e.def.options.flatMap(e=>Array.from(e._zod.values)))}),V(e,`pattern`,e=>{if(e.def.options.every(e=>e._zod.pattern)){let t=e.def.options.map(e=>e._zod.pattern);return RegExp(`^(${t.map(e=>Xe(e.source)).join(`|`)})$`)}});let n=t.options.length===1?t.options[0]._zod.run:null;e._zod.parse=(r,i)=>{if(n)return n(r,i);let a=!1,o=[];for(let e of t.options){let t=e._zod.run({value:r.value,issues:[]},i);if(t instanceof Promise)o.push(t),a=!0;else{if(t.issues.length===0)return t;o.push(t)}}return a?Promise.all(o).then(t=>Di(t,r,e,i)):Di(o,r,e,i)}}),ki=H(`$ZodIntersection`,(e,t)=>{K.init(e,t),e._zod.parse=(e,n)=>{let r=e.value,i=t.left._zod.run({value:r,issues:[]},n),a=t.right._zod.run({value:r,issues:[]},n);return i instanceof Promise||a instanceof Promise?Promise.all([i,a]).then(([t,n])=>ji(e,t,n)):ji(e,i,a)}});function Ai(e,t){if(e===t||e instanceof Date&&t instanceof Date&&+e==+t)return{valid:!0,data:e};if(lt(e)&&lt(t)){let n=Object.keys(t),r=Object.keys(e).filter(e=>n.indexOf(e)!==-1),i={...e,...t};Object.prototype.hasOwnProperty.call(i,`__proto__`)&&delete i.__proto__;for(let n of r){if(n===`__proto__`)continue;let r=Ai(e[n],t[n]);if(!r.valid)return{valid:!1,mergeErrorPath:[n,...r.mergeErrorPath]};i[n]=r.data}return{valid:!0,data:i}}if(Array.isArray(e)&&Array.isArray(t)){if(e.length!==t.length)return{valid:!1,mergeErrorPath:[]};let n=[];for(let r=0;r<e.length;r++){let i=e[r],a=t[r],o=Ai(i,a);if(!o.valid)return{valid:!1,mergeErrorPath:[r,...o.mergeErrorPath]};n.push(o.data)}return{valid:!0,data:n}}return{valid:!1,mergeErrorPath:[]}}function ji(e,t,n){let r=new Map,i,a=new Map,o=(e,t)=>{let n;if(e.code===`unrecognized_keys`&&!e.path?.length)i??=e,n=e.keys;else if(e.code===`invalid_key`&&e.origin===`record`&&e.path?.length===1){let t=String(e.path[0]);a.has(t)||a.set(t,e),n=[t]}else return!1;for(let e of n)r.has(e)||r.set(e,{}),r.get(e)[t]=!0;return!0};for(let n of t.issues)o(n,`l`)||e.issues.push(n);for(let t of n.issues)o(t,`r`)||e.issues.push(t);let s=[...r].filter(([,e])=>e.l&&e.r).map(([e])=>e);if(s.length){let t=i?s.filter(e=>i.keys.includes(e)):[];t.length&&e.issues.push({...i,keys:t});for(let n of s)!t.includes(n)&&a.has(n)&&e.issues.push(a.get(n))}let c=Ai(t.value,n.value);if(!c.valid){if(z(e))return e;throw Error(`Unmergable intersection. Error path: ${JSON.stringify(c.mergeErrorPath)}`)}return e.value=c.data,e}var Mi=H(`$ZodEnum`,(e,t)=>{K.init(e,t);let n=We(t.entries),r=new Set(n);e._zod.values=r,V(e,`pattern`,e=>{let t=We(e.def.entries).filter(e=>dt.has(typeof e));return RegExp(t.length?`^(${t.map(e=>ft(e.toString())).join(`|`)})$`:`^[^\\s\\S]$`)}),e._zod.parse=(t,i)=>{let a=t.value;return r.has(a)||t.issues.push({code:`invalid_value`,values:n,input:a,inst:e}),t}}),Ni=H(`$ZodTransform`,(e,t)=>{K.init(e,t),e._zod.optin=`optional`,U.memoizer?.guard(e),e._zod.parse=(n,r)=>{if(r.direction===`backward`)throw new $t(e.constructor.name);let i=t.transform(n.value,n);if(r.async)return(i instanceof Promise?i:Promise.resolve(i)).then(e=>(n.value=e,n));if(i instanceof Promise)throw new Qt;return n.value=i,n}});function Pi(e,t){return e.value=t.issues.length?void 0:t.value,e}var Fi=H(`$ZodOptional`,(e,t)=>{K.init(e,t),V(e,`optin`,e=>e.def.innerType._zod.optin===`defaulted`?`defaulted`:`optional`),e._zod.optout=`optional`,V(e,`values`,e=>{let t=e.def.innerType._zod.values;return t?new Set([...t,void 0]):void 0}),V(e,`pattern`,e=>{let t=e.def.innerType._zod.pattern;return t?RegExp(`^(${Xe(t.source)})?$`):void 0}),e._zod.parse=(e,n)=>{if(e.value===void 0){if(t.innerType._zod.optin!==`defaulted`)return e;let r=t.innerType._zod.run({value:e.value,issues:[]},n);return r instanceof Promise?r.then(t=>Pi(e,t)):Pi(e,r)}return t.innerType._zod.run(e,n)}}),Ii=H(`$ZodExactOptional`,(e,t)=>{Fi.init(e,t),V(e,`values`,e=>e.def.innerType._zod.values),V(e,`pattern`,e=>e.def.innerType._zod.pattern),e._zod.parse=(e,n)=>t.innerType._zod.run(e,n)}),Li=H(`$ZodNullable`,(e,t)=>{K.init(e,t),V(e,`optin`,e=>e.def.innerType._zod.optin),V(e,`optout`,e=>e.def.innerType._zod.optout),V(e,`pattern`,e=>{let t=e.def.innerType._zod.pattern;return t?RegExp(`^(${Xe(t.source)}|null)$`):void 0}),V(e,`values`,e=>e.def.innerType._zod.values?new Set([...e.def.innerType._zod.values,null]):void 0),e._zod.parse=(e,n)=>e.value===null?e:t.innerType._zod.run(e,n)}),Ri=H(`$ZodDefault`,(e,t)=>{K.init(e,t),e._zod.optin=`defaulted`,V(e,`values`,e=>e.def.innerType._zod.values),e._zod.parse=(e,n)=>{if(n.direction===`backward`)return t.innerType._zod.run(e,n);if(e.value===void 0)return e.value=t.defaultValue,e;let r=t.innerType._zod.run(e,n);return r instanceof Promise?r.then(e=>zi(e,t)):zi(r,t)}});function zi(e,t){return e.value===void 0&&(e.value=t.defaultValue),e}var Bi=H(`$ZodPrefault`,(e,t)=>{K.init(e,t),e._zod.optin=`defaulted`,V(e,`values`,e=>e.def.innerType._zod.values),e._zod.parse=(e,n)=>(n.direction===`backward`||e.value===void 0&&(e.value=t.defaultValue),t.innerType._zod.run(e,n))}),Vi=H(`$ZodNonOptional`,(e,t)=>{K.init(e,t),V(e,`values`,e=>{let t=e.def.innerType._zod.values;return t?new Set([...t].filter(e=>e!==void 0)):void 0}),e._zod.parse=(n,r)=>{let i=t.innerType._zod.run(n,r);return i instanceof Promise?i.then(t=>Hi(t,e)):Hi(i,e)}});function Hi(e,t){return!e.issues.length&&e.value===void 0&&e.issues.push({code:`invalid_type`,expected:`nonoptional`,input:e.value,inst:t}),e}function Ui(e,t,n,r){return t.issues.length?(e.value=n.catchValue({...t,value:e.value,error:{issues:t.issues.map(e=>jt(e,r,W()))},input:e.value}),e):(e.value=t.value,t.memo&&(e.memo=!0),e)}var Wi=H(`$ZodCatch`,(e,t)=>{K.init(e,t),V(e,`optin`,e=>e.def.innerType._zod.optin===`defaulted`?`defaulted`:`optional`),V(e,`optout`,e=>e.def.innerType._zod.optout),V(e,`values`,e=>e.def.innerType._zod.values),e._zod.parse=(e,n)=>{if(n.direction===`backward`)return t.innerType._zod.run(e,n);let r=t.innerType._zod.run({value:e.value,issues:[]},n);return r instanceof Promise?r.then(r=>Ui(e,r,t,n)):Ui(e,r,t,n)}}),Gi=H(`$ZodPipe`,(e,t)=>{K.init(e,t),V(e,`values`,e=>e.def.in._zod.values),V(e,`optin`,e=>e.def.in._zod.optin),V(e,`optout`,e=>e.def.out._zod.optout),V(e,`propValues`,e=>e.def.in._zod.propValues),e._zod.parse=(e,n)=>{if(n.direction===`backward`){let r=t.out._zod.run(e,n);return r instanceof Promise?r.then(e=>Ki(e,t.in,n)):Ki(r,t.in,n)}let r=t.in._zod.run(e,n);return r instanceof Promise?r.then(e=>Ki(e,t.out,n)):Ki(r,t.out,n)}});function Ki(e,t,n){return e.issues.some(e=>e.code!==`unrecognized_keys`)?(e.aborted=!0,e):t._zod.run({value:e.value,issues:e.issues},n)}var qi=H(`$ZodReadonly`,(e,t)=>{K.init(e,t),V(e,`propValues`,e=>e.def.innerType._zod.propValues),V(e,`values`,e=>e.def.innerType._zod.values),V(e,`optin`,e=>e.def.innerType?._zod?.optin),V(e,`optout`,e=>e.def.innerType?._zod?.optout),e._zod.parse=(e,n)=>{if(n.direction===`backward`)return t.innerType._zod.run(e,n);let r=t.innerType._zod.run(e,n);return r instanceof Promise?r.then(Ji):Ji(r)}});function Ji(e){return e.memo||(e.value=Object.freeze(e.value)),e}var Yi=H(`$ZodCustom`,(e,t)=>{G.init(e,t),K.init(e,t),e._zod.parse=(e,t)=>e,e._zod.check=n=>{let r=n.value,i=t.fn(r);if(i instanceof Promise)return i.then(t=>Xi(t,n,r,e));Xi(i,n,r,e)}});function Xi(e,t,n,r){if(!e){let e={code:`custom`,input:n,inst:r,path:[...r._zod.def.path??[]],continue:!r._zod.def.abort};r._zod.def.params&&(e.params=r._zod.def.params),t.issues.push(It(e))}}var Zi=class extends Error{constructor(){super(`Cannot parse a reference cycle that closes through a transform`),this.name=`ZodCyclicError`}},Qi=`~memo`,$i=[];function ea(e){return typeof e==`object`&&!!e}function ta(e){return e.map(e=>e.path?{...e,path:e.path.slice()}:{...e})}var na=new WeakMap,ra=0,ia=1,aa=2;function oa(e,t,n){let r=na.get(e);if(r!==void 0)return r?aa:ra;if(t.has(e))return aa;t.add(e);let i=ra,a=e=>{if(i!==aa&&e?._zod){let r=oa(e,t,n);r>i&&(i=r)}},o=(e,r)=>{let i=ra;for(let a of Reflect.ownKeys(e)){let o=Object.getOwnPropertyDescriptor(e,a);if(r&&!o.enumerable)continue;let s=o.get?ia:o.value?._zod?oa(o.value,t,n):ra;s>i&&(i=s)}return i},s=e=>{e>i&&(i=e)},c=e._zod.def;switch(c.type){case`object`:{let e=$e(c);s(e?o(e,!0):ia),a(c.catchall);break}case`array`:a(c.element);break;case`tuple`:for(let e of c.items)a(e);a(c.rest);break;case`record`:case`map`:a(c.keyType),a(c.valueType);break;case`set`:a(c.valueType);break;case`union`:for(let e of c.options)a(e);break;case`intersection`:a(c.left),a(c.right);break;case`optional`:case`nullable`:case`default`:case`prefault`:case`catch`:case`readonly`:case`nonoptional`:case`promise`:case`success`:a(c.innerType);break;case`pipe`:a(c.in),a(c.out);break;case`function`:a(c.input),a(c.output);break;case`lazy`:{let r=c._cachedInner??(n?e._zod.innerType:void 0);s(r?oa(r,t,!1):ia);break}case`template_literal`:case`string`:case`number`:case`int`:case`boolean`:case`bigint`:case`symbol`:case`undefined`:case`null`:case`void`:case`never`:case`any`:case`unknown`:case`date`:case`nan`:case`enum`:case`literal`:case`file`:case`transform`:case`custom`:break;default:for(let e in c){let t=Object.getOwnPropertyDescriptor(c,e);if(!t||t.get)continue;let n=t.value;if(n&&typeof n==`object`){if(n._zod)a(n);else if(Array.isArray(n))for(let e of n)a(e)}}}return t.delete(e),sa(e,i)}function sa(e,t){return t!==ia&&na.set(e,t===aa),t}function ca(e,t){let n=e.buckets.get(t);return n||(n=new WeakMap,e.buckets.set(t,n)),n}var la,ua=[],da={alloc(e,t,n){let r=la;if(!r)return n;la=void 0;let i={value:n,issues:null};return r.set(t.value,i),ua.push(i),n},guard(e){var t;(t=e._zod).deferred??(t.deferred=[]),e._zod.deferred.push(()=>{let t=e._zod.parse,n=(e,n)=>{if(n.direction!==`backward`&&pa(n,e.value))throw new Zi;return t(e,n)};e._zod.parse=n,e._zod.run===t&&(e._zod.run=n)})},attach(e){var t;let n,r=!1,i,a;(t=e._zod).deferred??(t.deferred=[]),e._zod.deferred.push(()=>{let t=e._zod.parse,o=(s,c)=>{if(n===void 0){let i=oa(e,new Set,!1);if(i===ra)return e._zod.parse=t,e._zod.run===o&&(e._zod.run=t),t(s,c);i===aa||r?n=!0:r=!0}let l=s.value;if(!ea(l))return t(s,c);let u=c[Qi];u||(u={buckets:new WeakMap,backEdges:void 0},c[Qi]=u);let d;i===c?d=a:(d=ca(u,e),i=c,a=d);let f=d.get(l);if(f)return s.value=f.value,f.issues?f.issues.length&&s.issues.push(...ta(f.issues)):(s.memo=!0,u.backEdges??(u.backEdges=new WeakSet),u.backEdges.add(f.value)),s;la=d;let p=ua.length,m=t(s,c);la=void 0;let h=ua.length>p?ua.pop():void 0;return m instanceof Promise?m.then(e=>(h&&(h.issues=e.issues.length?ta(e.issues):$i),e)):(h&&(h.issues=m.issues.length?ta(m.issues):$i),m)};e._zod.parse=o,e._zod.run===t&&(e._zod.run=o)})}};function fa(){return da}function pa(e,t){let n=e[Qi]?.backEdges;return n!==void 0&&ea(t)&&n.has(t)}var ma=()=>{let e={string:{unit:`characters`,verb:`to have`},file:{unit:`bytes`,verb:`to have`},array:{unit:`items`,verb:`to have`},set:{unit:`items`,verb:`to have`},map:{unit:`entries`,verb:`to have`}};function t(t){return e[t]??null}let n={regex:`input`,email:`email address`,url:`URL`,emoji:`emoji`,uuid:`UUID`,uuidv4:`UUIDv4`,uuidv6:`UUIDv6`,nanoid:`nanoid`,guid:`GUID`,cuid:`cuid`,cuid2:`cuid2`,ulid:`ULID`,xid:`XID`,ksuid:`KSUID`,datetime:`ISO datetime`,date:`ISO date`,time:`ISO time`,duration:`ISO duration`,ipv4:`IPv4 address`,ipv6:`IPv6 address`,mac:`MAC address`,cidrv4:`IPv4 range`,cidrv6:`IPv6 range`,base64:`base64-encoded string`,base64url:`base64url-encoded string`,json_string:`JSON string`,e164:`E.164 number`,currency_code:`currency code`,credit_card:`credit card number`,iban:`IBAN`,jwt:`JWT`,template_literal:`input`},r={nan:`NaN`};function i(e,t){return e===`number`&&typeof t==`number`&&!Number.isFinite(t)?String(t):r[e]??e}return e=>{switch(e.code){case`invalid_type`:return`Invalid input: expected ${i(e.expected)}, received ${i(Ft(e.input),e.input)}`;case`invalid_value`:return e.values.length===1?`Invalid input: expected ${pt(e.values[0])}`:`Invalid option: expected one of ${Ge(e.values,`|`)}`;case`too_big`:{let n=e.exact?`exactly `:e.inclusive?`<=`:`<`,r=t(e.origin);return r?`Too big: expected ${e.origin??`value`} to have ${n}${e.maximum.toString()} ${r.unit??`elements`}`:`Too big: expected ${e.origin??`value`} to be ${n}${e.maximum.toString()}`}case`too_small`:{let n=e.exact?`exactly `:e.inclusive?`>=`:`>`,r=t(e.origin);return r?`Too small: expected ${e.origin} to have ${n}${e.minimum.toString()} ${r.unit}`:`Too small: expected ${e.origin} to be ${n}${e.minimum.toString()}`}case`invalid_format`:{let t=e;return t.format===`starts_with`?`Invalid string: must start with "${t.prefix}"`:t.format===`ends_with`?`Invalid string: must end with "${t.suffix}"`:t.format===`includes`?`Invalid string: must include "${t.includes}"`:t.format===`regex`?`Invalid string: must match pattern ${t.pattern}`:`Invalid ${n[t.format]??e.format}`}case`not_multiple_of`:return`Invalid number: must be a multiple of ${e.divisor}`;case`unrecognized_keys`:return`Unrecognized key${e.keys.length>1?`s`:``}: ${Ge(e.keys,`, `)}`;case`invalid_key`:return`Invalid key in ${e.origin}`;case`invalid_union`:return e.options&&Array.isArray(e.options)&&e.options.length>0?`Invalid discriminator value. Expected ${e.options.map(e=>`'${e}'`).join(` | `)}`:e.inclusive===!1?`Invalid input: more than one option matched`:`Invalid input`;case`invalid_element`:return`Invalid value in ${e.origin}`;default:return`Invalid input`}}};function ha(){return{localeError:ma()}}var ga,_a=class{constructor(){this._map=new WeakMap,this._idmap=new Map}add(e,...t){let n=t[0];return this._map.set(e,n),n&&typeof n==`object`&&`id`in n&&this._idmap.set(n.id,e),this}clear(){return this._map=new WeakMap,this._idmap=new Map,this}remove(e){let t=this._map.get(e);return t&&typeof t==`object`&&`id`in t&&this._idmap.delete(t.id),this._map.delete(e),this}get(e){let t=e._zod.parent;if(t){let n={...this.get(t)??{}};delete n.id;let r={...n,...this._map.get(e)};return Object.keys(r).length?r:void 0}return this._map.get(e)}has(e){return this._map.has(e)}};function va(){return new _a}(ga=globalThis).__zod_globalRegistry??(ga.__zod_globalRegistry=va());var ya=globalThis.__zod_globalRegistry;function ba(e){return e.checks&&=[...e.checks],e}function xa(e,t){return new e(ba({type:`string`,...R(t)}))}function Sa(e,t){return new e({type:`string`,format:`email`,check:`string_format`,abort:!1,...R(t)})}function Ca(e,t){return new e({type:`string`,format:`guid`,check:`string_format`,abort:!1,...R(t)})}function wa(e,t){return new e({type:`string`,format:`uuid`,check:`string_format`,abort:!1,...R(t)})}function Ta(e,t){return new e({type:`string`,format:`uuid`,check:`string_format`,abort:!1,version:`v4`,...R(t)})}function Ea(e,t){return new e({type:`string`,format:`uuid`,check:`string_format`,abort:!1,version:`v6`,...R(t)})}function Da(e,t){return new e({type:`string`,format:`uuid`,check:`string_format`,abort:!1,version:`v7`,...R(t)})}function Oa(e,t){return new e({type:`string`,format:`url`,check:`string_format`,abort:!1,...R(t)})}function ka(e,t){return new e({type:`string`,format:`emoji`,check:`string_format`,abort:!1,...R(t)})}function Aa(e,t){return new e({type:`string`,format:`nanoid`,check:`string_format`,abort:!1,...R(t)})}function ja(e,t){return new e({type:`string`,format:`cuid`,check:`string_format`,abort:!1,...R(t)})}function Ma(e,t){return new e({type:`string`,format:`cuid2`,check:`string_format`,abort:!1,...R(t)})}function Na(e,t){return new e({type:`string`,format:`ulid`,check:`string_format`,abort:!1,...R(t)})}function Pa(e,t){return new e({type:`string`,format:`xid`,check:`string_format`,abort:!1,...R(t)})}function Fa(e,t){return new e({type:`string`,format:`ksuid`,check:`string_format`,abort:!1,...R(t)})}function Ia(e,t){return new e({type:`string`,format:`ipv4`,check:`string_format`,abort:!1,...R(t)})}function La(e,t){return new e({type:`string`,format:`ipv6`,check:`string_format`,abort:!1,...R(t)})}function Ra(e,t){return new e({type:`string`,format:`cidrv4`,check:`string_format`,abort:!1,...R(t)})}function za(e,t){return new e({type:`string`,format:`cidrv6`,check:`string_format`,abort:!1,...R(t)})}function Ba(e,t){return new e({type:`string`,format:`base64`,check:`string_format`,abort:!1,...R(t)})}function Va(e,t){return new e({type:`string`,format:`base64url`,check:`string_format`,abort:!1,...R(t)})}function Ha(e,t){return new e({type:`string`,format:`e164`,check:`string_format`,abort:!1,...R(t)})}function Ua(e,t){return new e({type:`string`,format:`jwt`,check:`string_format`,abort:!1,...R(t)})}function Wa(e,t){return new e({type:`string`,format:`datetime`,check:`string_format`,offset:!1,local:!1,precision:null,...R(t)})}function Ga(e,t){return new e({type:`string`,format:`date`,check:`string_format`,...R(t)})}function Ka(e,t){return new e({type:`string`,format:`time`,check:`string_format`,precision:null,...R(t)})}function qa(e,t){return new e({type:`string`,format:`duration`,check:`string_format`,...R(t)})}function Ja(e,t){return new e(ba({type:`number`,checks:[],...R(t)}))}function Ya(e,t){return new e({type:`number`,check:`number_format`,abort:!1,format:`safeint`,...R(t)})}function Xa(e){return new e({type:`unknown`})}function Za(e,t){return new e({type:`never`,...R(t)})}function Qa(e,t){return new cr({check:`less_than`,...R(t),value:e,inclusive:!1})}function $a(e,t){return new cr({check:`less_than`,...R(t),value:e,inclusive:!0})}function eo(e,t){return new lr({check:`greater_than`,...R(t),value:e,inclusive:!1})}function to(e,t){return new lr({check:`greater_than`,...R(t),value:e,inclusive:!0})}function no(e,t){return new ur({check:`multiple_of`,...R(t),value:e})}function ro(e,t){return new fr({check:`max_length`,...R(t),maximum:e})}function io(e,t){return new pr({check:`min_length`,...R(t),minimum:e})}function ao(e,t){return new mr({check:`length_equals`,...R(t),length:e})}function oo(e,t){return new gr({check:`string_format`,format:`regex`,...R(t),pattern:e})}function so(e){return new _r({check:`string_format`,format:`lowercase`,...R(e)})}function co(e){return new vr({check:`string_format`,format:`uppercase`,...R(e)})}function lo(e,t){return new yr({check:`string_format`,format:`includes`,...R(t),includes:e})}function uo(e,t){return new br({check:`string_format`,format:`starts_with`,...R(t),prefix:e})}function fo(e,t){return new xr({check:`string_format`,format:`ends_with`,...R(t),suffix:e})}function po(e){return new Sr({check:`overwrite`,tx:e})}function mo(e){return po(t=>t.normalize(e))}function ho(){return po(e=>e.trim())}function go(){return po(e=>e.toLowerCase())}function _o(){return po(e=>e.toUpperCase())}function vo(){return po(e=>at(e))}function yo(e,t,n){return new e({type:`array`,element:t,...R(n)})}function bo(e,t,n){return new e({type:`custom`,check:`custom`,fn:t,...R(n)})}function xo(e,t){let n=So(t=>(t.addIssue=e=>{if(typeof e==`string`)t.issues.push(It(e,t.value,n._zod.def));else{let r=e;r.fatal&&(r.continue=!1),r.code??=`custom`,`input`in r||(r.input=t.value),r.inst??=n,r.continue??=!n._zod.def.abort,t.issues.push(It(r))}},e(t.value,t)),t);return n}function So(e,t){let n=new G({check:`custom`,...R(t)});return n._zod.check=e,n}function Co(e,...t){for(let n of t)for(let t of Reflect.ownKeys(n))Object.prototype.propertyIsEnumerable.call(n,t)&&Qe(e,t,n[t]);return e}function wo(e){let t=e?.target??`draft-2020-12`;return t===`draft-4`&&(t=`draft-04`),t===`draft-7`&&(t=`draft-07`),{processors:e.processors??{},metadataRegistry:e?.metadata??ya,target:t,unrepresentable:e?.unrepresentable??`throw`,override:e?.override??(()=>{}),io:e?.io??`output`,counter:0,seen:new Map,sharedDefsExtractedFor:void 0,sharedEmitDoneFor:void 0,cycles:e?.cycles??`ref`,reused:e?.reused??`inline`,intersections:[],deferred:[],external:e?.external??void 0}}function To(e,t,n,r,i){let a=typeof t.unrepresentable==`function`?t.unrepresentable({zodSchema:e,path:r.path,message:i}):t.unrepresentable;if(a===`any`)return!1;if(a===void 0||a===`throw`)throw Error(i);return Object.assign(n,a),!0}function J(e,t,n={path:[],schemaPath:[]}){var r;let i=e._zod.def,a=t.seen.get(e);if(a)return a.count++,n.schemaPath.includes(e)&&(a.cycle=n.path),a.schema;let o={schema:{},count:1,cycle:void 0,path:n.path};t.seen.set(e,o),t.sharedDefsExtractedFor=void 0,t.sharedEmitDoneFor=void 0;let s=e._zod.toJSONSchema?.();if(s)o.schema=s;else{let r={...n,schemaPath:[...n.schemaPath,e],path:n.path};if(e._zod.processJSONSchema)e._zod.processJSONSchema(t,o.schema,r);else{let n=o.schema,a=t.processors[i.type];if(!a)throw Error(`[toJSONSchema]: Non-representable type encountered: ${i.type}`);a(e,t,n,r)}let a=e._zod.parent;a&&(o.ref||=a,J(a,t,r),t.seen.get(a).isParent=!0)}let c=t.metadataRegistry.get(e);return c&&Co(o.schema,c),t.io===`input`&&Y(e)&&(delete o.schema.examples,delete o.schema.default),t.io===`input`&&`_prefault`in o.schema&&((r=o.schema).default??(r.default=o.schema._prefault)),delete o.schema._prefault,t.seen.get(e).schema}function Eo(e){return e.replace(/~/g,`~0`).replace(/\//g,`~1`)}function Do(e,t){let n=e.seen.get(t);if(!n)throw Error(`Unprocessed schema. This is a bug in Zod.`);if(e.external&&e.sharedDefsExtractedFor===e.external)return;let r=new Map;for(let t of e.seen.entries()){let n=e.metadataRegistry.get(t[0])?.id;if(n){let e=r.get(n);if(e&&e!==t[0])throw Error(`Duplicate schema id "${n}" detected during JSON Schema conversion. Two different schemas cannot share the same id when converted together.`);r.set(n,t[0])}}let i=t=>{let r=e.target===`draft-2020-12`?`$defs`:`definitions`;if(e.external){let n=e.external.registry.get(t[0])?.id,i=e.external.uri??(e=>e);if(n)return{ref:i(n)};let a=t[1].defId??t[1].schema.id??`schema${e.counter++}`;return t[1].defId=a,{defId:a,ref:`${i(`__shared`)}#/${r}/${Eo(a)}`}}let i=`#/${r}/`;if(t[1]===n&&!t[1].schema.id)return{ref:`#`};let a=t[1].schema.id??`__schema${e.counter++}`;return{defId:a,ref:i+Eo(a)}},a=e=>{if(e[1].schema.$ref)return;let t=e[1],{ref:n,defId:r}=i(e);t.def={...t.schema},r&&(t.defId=r);let a=t.schema;for(let e in a)delete a[e];a.$ref=n};if(e.cycles===`throw`)for(let t of e.seen.entries()){let e=t[1];if(e.cycle)throw Error(`Cycle detected: #/${e.cycle?.join(`/`)}/<root>

Set the \`cycles\` parameter to \`"ref"\` to resolve cyclical schemas with defs.`)}for(let n of e.seen.entries()){let r=n[1];if(t===n[0])a(n);else{if(e.external){let r=e.external.registry.get(n[0])?.id;if(t!==n[0]&&r){a(n);continue}}(e.metadataRegistry.get(n[0])?.id||r.cycle||r.count>1&&e.reused===`ref`)&&a(n)}}e.external&&(e.sharedDefsExtractedFor=e.external)}function Oo(e){let t=e.anyOf;if(!Array.isArray(t)||t.length===0||e.type!==void 0)return;let n=[];for(let e of t){if(!e||typeof e!=`object`)return;Oo(e);let t=Object.keys(e);if(t.length!==1||t[0]!==`type`)return;let r=e.type;for(let e of Array.isArray(r)?r:[r]){if(typeof e!=`string`)return;n.includes(e)||n.push(e)}}delete e.anyOf,e.type=n.length===1?n[0]:n}var ko=new Set([`type`,`properties`,`required`,`additionalProperties`]),Ao=[`oneOf`,`anyOf`];function jo(e){let t=e.additionalProperties;return t===void 0||t===!1||typeof t!=`object`||!t?null:Object.keys(t).length?t:null}function Mo(e){let t=[];for(let n of e){if(typeof n!=`object`||n.type!==`object`)return null;for(let e in n)if(!ko.has(e))return null;t.push(n)}let n={},r=new Set;for(let e of t){for(let r in e.properties){if(Object.prototype.hasOwnProperty.call(n,r))continue;let e=[];for(let n of t){let t=n.properties?.[r]??jo(n);t!=null&&(e.some(e=>JSON.stringify(e)===JSON.stringify(t))||e.push(t))}Qe(n,r,e.length===1?e[0]:Mo(e)??{allOf:e})}for(let t of e.required??[])r.add(t)}let i={type:`object`,properties:n};if(r.size&&(i.required=[...r]),t.every(e=>e.additionalProperties===!1))i.additionalProperties=!1;else{let e=[];for(let n of t){let t=jo(n);t&&!e.some(e=>JSON.stringify(e)===JSON.stringify(t))&&e.push(t)}e.length===1?i.additionalProperties=e[0]:e.length>1&&(i.additionalProperties={allOf:e})}return i}function No(e){let t=e.allOf;if(!Array.isArray(t)||t.length<2)return;for(let t of ko)if(t in e)return;let n=t.filter(e=>Ao.some(t=>Array.isArray(e[t]))),r=null;if(!n.length)r=Mo(t);else{let e=n[0],i=Ao.find(t=>Array.isArray(e[t]));if(Object.keys(e).length!==1)return;let a=t.filter(t=>t!==e),o=e[i].map(e=>Mo([...a,e]));if(o.some(e=>!e))return;r={[i]:o}}r&&(delete e.allOf,Co(e,r))}function Po(e,t){let n=e.seen.get(t);if(!n)throw Error(`Unprocessed schema. This is a bug in Zod.`);let r=t=>{let n=e.seen.get(t);if(n.ref===null)return;let i=n.def??n.schema,a={...i},o=n.ref;if(n.ref=null,o){r(o);let n=e.seen.get(o),s=n.schema;if(s.$ref&&(e.target===`draft-07`||e.target===`draft-04`||e.target===`openapi-3.0`)?(i.allOf=i.allOf??[],i.allOf.push(s)):Co(i,s),Co(i,a),t._zod.parent===o)for(let e in i)e!==`$ref`&&e!==`allOf`&&(e in a||delete i[e]);if(s.$ref&&n.def)for(let e in i)e!==`$ref`&&e!==`allOf`&&e in n.def&&JSON.stringify(i[e])===JSON.stringify(n.def[e])&&delete i[e]}let s=t._zod.parent;if(s&&s!==o){r(s);let t=e.seen.get(s);if(t?.schema.$ref&&(i.$ref=t.schema.$ref,t.def))for(let e in i)e!==`$ref`&&e!==`allOf`&&e in t.def&&JSON.stringify(i[e])===JSON.stringify(t.def[e])&&delete i[e]}e.override({zodSchema:t,jsonSchema:i,path:n.path??[]})};if(!e.external||e.sharedEmitDoneFor!==e.external){for(let t of[...e.seen.entries()].reverse())r(t[0]);if(e.target!==`openapi-3.0`)for(let t of e.seen.entries())Oo(t[1].def??t[1].schema);for(let t of e.deferred)t();if(e.intersections.length){let t=new Map;for(let n of e.seen.values())for(let e of[n.schema,n.def]){let n=e?.allOf;if(!Array.isArray(n))continue;let r=t.get(n);r?r.push(e):t.set(n,[e])}for(let n of e.intersections)for(let e of t.get(n)??[])No(e)}}let i={};if(e.target===`draft-2020-12`?i.$schema=`https://json-schema.org/draft/2020-12/schema`:e.target===`draft-07`?i.$schema=`http://json-schema.org/draft-07/schema#`:e.target===`draft-04`?i.$schema=`http://json-schema.org/draft-04/schema#`:e.target,e.external?.uri){let n=e.external.registry.get(t)?.id;if(!n)throw Error("Schema is missing an `id` property");i.$id=e.external.uri(n)}Co(i,n.defId?n.schema:n.def??n.schema);let a=e.metadataRegistry.get(t)?.id;a!==void 0&&i.id===a&&delete i.id;let o=e.external?.defs??{};if(!e.external||e.sharedEmitDoneFor!==e.external)for(let t of e.seen.entries()){let e=t[1];e.def&&e.defId&&(e.def.id===e.defId&&delete e.def.id,Qe(o,e.defId,e.def))}e.external&&(e.sharedEmitDoneFor=e.external),e.external||Object.keys(o).length>0&&(e.target===`draft-2020-12`?i.$defs=o:i.definitions=o);try{let n=JSON.parse(JSON.stringify(i));return Object.defineProperty(n,"~standard",{value:{...t[`~standard`],jsonSchema:{input:Io(t,`input`,e.processors),output:Io(t,`output`,e.processors)}},enumerable:!1,writable:!1}),n}catch{throw Error(`Error converting schema to JSON.`)}}function Y(e,t){let n=t??{seen:new Set};if(n.seen.has(e))return!1;n.seen.add(e);let r=e._zod.def;if(r.type===`transform`)return!0;if(r.type===`array`)return Y(r.element,n);if(r.type===`set`)return Y(r.valueType,n);if(r.type===`lazy`)return Y(r.getter(),n);if(r.type===`promise`||r.type===`optional`||r.type===`nonoptional`||r.type===`nullable`||r.type===`readonly`||r.type==="default"||r.type===`prefault`||r.type===`catch`)return Y(r.innerType,n);if(r.type===`intersection`)return Y(r.left,n)||Y(r.right,n);if(r.type===`record`||r.type===`map`)return Y(r.keyType,n)||Y(r.valueType,n);if(r.type===`pipe`)return e._zod.traits.has(`$ZodCodec`)?!0:Y(r.in,n)||Y(r.out,n);if(r.type===`object`){for(let e in r.shape)if(Y(r.shape[e],n))return!0;return!1}if(r.type===`union`){for(let e of r.options)if(Y(e,n))return!0;return!1}if(r.type===`tuple`){for(let e of r.items)if(Y(e,n))return!0;return!!(r.rest&&Y(r.rest,n))}return!1}var Fo=(e,t={})=>n=>{let r=wo({...n,processors:t});return J(e,r),Do(r,e),Po(r,e)},Io=(e,t,n={})=>r=>{let{libraryOptions:i,target:a}=r??{},o=wo({...i??{},target:a,io:t,processors:n});return J(e,o),Do(o,e),Po(o,e)},Lo=(e,t,n)=>{(e[t]===void 0||n>e[t])&&(e[t]=n)},Ro=(e,t,n)=>{(e[t]===void 0||n<e[t])&&(e[t]=n)},zo=(e,t)=>{Lo(e,`minimum`,t),Ro(e,`maximum`,t)},Bo=(e,t)=>{e.multipleOf??=[],e.multipleOf.includes(t)||e.multipleOf.push(t)},Vo=(e,t)=>{e.patterns??=new Set,e.patterns.add(t)},Ho=(e,t)=>{e.mime=e.mime?e.mime.filter(e=>t.includes(e)):[...t]},Uo=(e,t)=>{e.format=t,t.includes(`int`)&&(e.isInt=!0)},Wo=(e,t)=>Lo(e,`minimum`,t.minimum),Go=(e,t)=>Ro(e,`maximum`,t.maximum),Ko=e=>(t,n)=>{Uo(t,n.format);let[r,i]=e[n.format];Lo(t,`minimum`,r),Ro(t,`maximum`,i)},qo={greater_than:(e,t)=>Lo(e,t.inclusive?`minimum`:`exclusiveMinimum`,t.value),less_than:(e,t)=>Ro(e,t.inclusive?`maximum`:`exclusiveMaximum`,t.value),multiple_of:(e,t)=>Bo(e,t.value),number_format:Ko(ht),bigint_format:Ko(gt),min_length:Wo,max_length:Go,length_equals:(e,t)=>zo(e,t.length),min_size:Wo,max_size:Go,size_equals:(e,t)=>zo(e,t.size),string_format:(e,t)=>{Uo(e,t.format),t.pattern&&Vo(e,t.pattern),(t.format===`base64`||t.format===`base64url`)&&(e.contentEncoding=t.format),(t.local||t.precision===-1)&&(e.laxFormat=!0)},mime_type:(e,t)=>Ho(e,t.mime)};function X(e){let t={},n=e._zod.def,r=e._zod.traits.has(`$ZodCheck`)?[e,...n.checks??[]]:n.checks??[];for(let e of r)qo[e._zod.def.check]?.(t,e._zod.def);let i=e._zod.bag;i.minimum!==void 0&&Lo(t,`minimum`,i.minimum),i.exclusiveMinimum!==void 0&&Lo(t,`exclusiveMinimum`,i.exclusiveMinimum),i.maximum!==void 0&&Ro(t,`maximum`,i.maximum),i.exclusiveMaximum!==void 0&&Ro(t,`exclusiveMaximum`,i.exclusiveMaximum),i.multipleOf!==void 0&&Bo(t,i.multipleOf),i.format!==void 0&&(t.format??=i.format,i.format.includes(`int`)&&(t.isInt=!0)),i.mime&&Ho(t,i.mime);for(let e of i.patterns??[])Vo(t,e);return t}var Jo={guid:`uuid`,url:`uri`,datetime:`date-time`,json_string:`json-string`,regex:``},Yo=new Map([[ai,Kn],[si,qn]]),Xo=e=>Yo.get(e)??e,Zo=(e,t,n,r)=>{let i=n;i.type=`string`;let{minimum:a,maximum:o,format:s,patterns:c,contentEncoding:l,laxFormat:u}=X(e);if(typeof a==`number`&&(i.minLength=a),typeof o==`number`&&(i.maxLength=o),s&&(i.format=Jo[s]??s,i.format===``&&delete i.format,(s===`time`||u)&&delete i.format),l&&(i.contentEncoding=l),c&&c.size>0){let e=[...c].map(Xo);e.length===1?i.pattern=e[0].source:e.length>1&&(i.allOf=[...e.map(e=>({...t.target===`draft-07`||t.target===`draft-04`||t.target===`openapi-3.0`?{type:`string`}:{},pattern:e.source}))])}},Qo=(e,t,n,r)=>{let i=n,{minimum:a,maximum:o,multipleOf:s,exclusiveMaximum:c,exclusiveMinimum:l,isInt:u}=X(e);i.type=u?`integer`:`number`;let d=typeof l==`number`&&l>=(a??-1/0),f=typeof c==`number`&&c<=(o??1/0),p=t.target===`draft-04`||t.target===`openapi-3.0`;if(d?p?(i.minimum=l,i.exclusiveMinimum=!0):i.exclusiveMinimum=l:typeof a==`number`&&(i.minimum=a),f?p?(i.maximum=c,i.exclusiveMaximum=!0):i.exclusiveMaximum=c:typeof o==`number`&&(i.maximum=o),s){let n=new Set;for(let a of s)Number.isFinite(a)&&a!==0?n.add(Math.abs(a)):To(e,t,i,r,`A multipleOf divisor of ${a} cannot be represented in JSON Schema`);let[a,...o]=n;a!==void 0&&(i.multipleOf=a),o.length&&(i.allOf=[...i.allOf??[],...o.map(e=>({multipleOf:e}))])}},$o=(e,t,n,r)=>{n.not={}},es=(e,t,n,r)=>{let i=e._zod.def,a=We(i.entries);a.length===0?n.not={}:(a.every(e=>typeof e==`number`)&&(n.type=`number`),a.every(e=>typeof e==`string`)&&(n.type=`string`),n.enum=a)},ts=(e,t,n,r)=>{To(e,t,n,r,`Custom types cannot be represented in JSON Schema`)},ns=(e,t,n,r)=>{To(e,t,n,r,`Transforms cannot be represented in JSON Schema`)},rs=(e,t,n,r)=>{let i=n,a=e._zod.def,{minimum:o,maximum:s}=X(e);typeof o==`number`&&(i.minItems=o),typeof s==`number`&&(i.maxItems=s),i.type=`array`,i.items=J(a.element,t,{...r,path:[...r.path,`items`]})};function is(e){let t=e._zod.def;return t.type===`pipe`&&t.in._zod.traits.has(`$ZodTransform`)?is(t.out):t.type===`catch`?is(t.innerType):e._zod.optin}var as=(e,t,n,r)=>{let i=n,a=e._zod.def,o=a.shape;if(Object.getOwnPropertySymbols(o).length&&To(e,t,i,r,`Symbol keys cannot be represented in JSON Schema`))return;i.type=`object`,i.properties={};for(let e in o)Qe(i.properties,e,J(o[e],t,{...r,path:[...r.path,`properties`,e]}));let s=[];for(let e of Object.keys(o)){let n=a.shape[e];(t.io===`input`?is(n)===void 0:n._zod.optout===void 0)&&s.push(e)}s.length>0&&(i.required=s),a.catchall?._zod.def.type===`never`?i.additionalProperties=!1:a.catchall?a.catchall&&(i.additionalProperties=J(a.catchall,t,{...r,path:[...r.path,`additionalProperties`]})):t.io===`output`&&(i.additionalProperties=!1)},os=(e,t,n,r)=>{let i=e._zod.def,a=i.inclusive===!1,o=i.options.map((e,n)=>J(e,t,{...r,path:[...r.path,a?`oneOf`:`anyOf`,n]}));a?n.oneOf=o:n.anyOf=o},ss=(e,t,n,r)=>{let i=e._zod.def,a=J(i.left,t,{...r,path:[...r.path,`allOf`,0]}),o=J(i.right,t,{...r,path:[...r.path,`allOf`,1]}),s=e=>`allOf`in e&&Object.keys(e).length===1,c=[...s(a)?a.allOf:[a],...s(o)?o.allOf:[o]];n.allOf=c,t.intersections.push(c)},cs=(e,t,n,r)=>{let i=e._zod.def,a=J(i.innerType,t,r),o=t.seen.get(e);t.target===`openapi-3.0`?(o.ref=i.innerType,n.nullable=!0):n.anyOf=[a,{type:`null`}]},ls=(e,t,n,r)=>{let i=e._zod.def;J(i.innerType,t,r);let a=t.seen.get(e);a.ref=i.innerType},us=Symbol();function ds(e,t,n,r,i){let a=!1,o=JSON.stringify(e,(e,t)=>typeof t==`bigint`?(a=!0,null):t);return a?(To(t,n,r,i,`BigInt defaults cannot be represented in JSON Schema`),us):JSON.parse(o)}var fs=(e,t,n,r)=>{let i=e._zod.def;J(i.innerType,t,r);let a=t.seen.get(e);a.ref=i.innerType;let o=ds(i.defaultValue,e,t,n,r);o!==us&&(n.default=o)},ps=(e,t,n,r)=>{let i=e._zod.def;J(i.innerType,t,r);let a=t.seen.get(e);if(a.ref=i.innerType,t.io!==`input`)return;let o=ds(i.defaultValue,e,t,n,r);o!==us&&(n._prefault=o)},ms=(e,t,n,r)=>{let i=e._zod.def;J(i.innerType,t,r);let a=t.seen.get(e);a.ref=i.innerType;let o;try{o=i.catchValue(void 0)}catch{To(e,t,n,r,`Dynamic catch values are not supported in JSON Schema`);return}n.default=o},hs=(e,t,n,r)=>{let i=e._zod.def,a=i.in._zod.traits.has(`$ZodTransform`),o=t.io===`input`?a?i.out:i.in:i.out;J(o,t,r);let s=t.seen.get(e);s.ref=o},gs=(e,t,n,r)=>{let i=e._zod.def;J(i.innerType,t,r);let a=t.seen.get(e);a.ref=i.innerType,n.readOnly=!0},_s=(e,t,n,r)=>{let i=e._zod.def;J(i.innerType,t,r);let a=t.seen.get(e);a.ref=i.innerType},vs=new WeakSet([Object.prototype,Error.prototype]);function ys(e,t,n){Object.defineProperty(e,t,{configurable:!0,enumerable:!1,get(){let e=n(this);return Object.defineProperty(this,t,{value:e,configurable:!0,writable:!0}),e},set(e){Object.defineProperty(this,t,{value:e,configurable:!0,writable:!0})}})}var Z=H(`ZodError`,(e,t)=>{on.init(e,t),e.name=`ZodError`;let n=Object.getPrototypeOf(e);vs.has(n)||(vs.add(n),ys(n,`format`,e=>t=>ln(e,t)),ys(n,`flatten`,e=>t=>cn(e,t)),ys(n,`addIssue`,e=>t=>{e.issues.push(t),e.message=JSON.stringify(e.issues,Ke,2)}),ys(n,`addIssues`,e=>t=>{e.issues.push(...t),e.message=JSON.stringify(e.issues,Ke,2)}),Object.defineProperty(n,"isEmpty",{configurable:!0,enumerable:!1,get(){return this.issues.length===0}}))},void 0,{Parent:Error}),bs=dn(Z),xs=fn(Z),Ss=pn(Z),Cs=hn(Z),ws=xn(Z),Ts=Sn(Z),Es=Cn(Z),Ds=wn(Z),Os=Tn(Z),ks=En(Z),As=Dn(Z),js=On(Z);function Ms(){U.localeError||W(ha())}function Ns(){U.memoizer||W({memoizer:fa()})}var Q=H(`ZodType`,(e,t)=>(Ms(),K.init(e,t),e.def=t,e.type=t.type,e),{check(...e){let t=this.def;return this.clone(I(t,{checks:[...t.checks??[],...e.map(e=>typeof e==`function`?{_zod:{check:e,def:{check:`custom`},onattach:[]}}:e)]}),{parent:!0})},with(...e){return this.check(...e)},clone(e,t){return L(this,e,t)},brand(){return this},register(e,t){return e.add(this,t),this},refine(e,t){return this.check(Gc(e,t))},superRefine(e,t){return this.check(Kc(e,t))},overwrite(e){return this.check(po(e))},optional(){return Dc(this)},exactOptional(){return kc(this)},nullable(){return jc(this)},nullish(){return Dc(jc(this))},nonoptional(e){return Lc(this,e)},array(){return hc(this)},or(e){return yc([this,e])},and(e){return xc(this,e)},transform(e){return Vc(this,Tc(e))},default(e){return Nc(this,e)},prefault(e){return Fc(this,e)},catch(e){return zc(this,e)},pipe(e){return Vc(this,e)},readonly(){return Uc(this)},describe(e){let t=this.clone();return ya.add(t,{description:e}),t},meta(...e){if(e.length===0)return ya.get(this);let t=this.clone();return ya.add(t,e[0]),t},isOptional(){return this.safeParse(void 0).success},isNullable(){return this.safeParse(null).success},apply(e,...t){return t.length===0?e(this):e(this,...t)},get"~standard"(){return Rt(this,`~standard`,{...Dr(this),jsonSchema:{input:Io(this,`input`),output:Io(this,`output`)}})},set"~standard"(e){B(this,`~standard`,e)},parse:function e(t,n){return bs(this,t,n,{callee:e})},parseAsync:async function e(t,n){return await xs(this,t,n,{callee:e})},safeParse(e,t){return Ss(this,e,t)},async safeParseAsync(e,t){return Cs(this,e,t)},get spa(){return this?.safeParseAsync},set spa(e){B(this,`spa`,e)},validate(e,t){return vn(this,e,t)},validateAsync(e,t){return bn(this,e,t)},encode:function e(t,n){return ws(this,t,n,{callee:e})},decode:function e(t,n){return Ts(this,t,n,{callee:e})},encodeAsync:async function e(t,n){return await Es(this,t,n,{callee:e})},decodeAsync:async function e(t,n){return await Ds(this,t,n,{callee:e})},safeEncode(e,t){return Os(this,e,t)},safeDecode(e,t){return ks(this,e,t)},async safeEncodeAsync(e,t){return As(this,e,t)},async safeDecodeAsync(e,t){return js(this,e,t)},toJSONSchema(e){return Fo(this,{})(e)},get description(){return ya.get(this)?.description},get _def(){return this._zod.def}}),Ps=H(`_ZodString`,(e,t)=>{Or.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>Zo(e,t,n,r)},zt({format:e=>X(e).format??null,minLength:e=>X(e).minimum??null,maxLength:e=>X(e).maximum??null},{regex(...e){return this.check(oo(...e))},includes(...e){return this.check(lo(...e))},startsWith(...e){return this.check(uo(...e))},endsWith(...e){return this.check(fo(...e))},min(...e){return this.check(io(...e))},max(...e){return this.check(ro(...e))},length(...e){return this.check(ao(...e))},nonempty(...e){return this.check(io(1,...e))},lowercase(e){return this.check(so(e))},uppercase(e){return this.check(co(e))},trim(){return this.check(ho())},normalize(...e){return this.check(mo(...e))},toLowerCase(){return this.check(go())},toUpperCase(){return this.check(_o())},slugify(){return this.check(vo())}})),Fs=H(`ZodString`,(e,t)=>{Or.init(e,t),Ps.init(e,t)},{email(e){return this.check(Sa(Vs,e))},url(e){return this.check(Oa(Ws,e))},jwt(e){return this.check(Ua(ac,e))},emoji(e){return this.check(ka(Gs,e))},guid(e){return this.check(Ca(Hs,e))},uuid(e){return this.check(wa(Us,e))},uuidv4(e){return this.check(Ta(Us,e))},uuidv6(e){return this.check(Ea(Us,e))},uuidv7(e){return this.check(Da(Us,e))},nanoid(e){return this.check(Aa(Ks,e))},cuid(e){return this.check(ja(qs,e))},cuid2(e){return this.check(Ma(Js,e))},ulid(e){return this.check(Na(Ys,e))},base64(e){return this.check(Ba(nc,e))},base64url(e){return this.check(Va(rc,e))},xid(e){return this.check(Pa(Xs,e))},ksuid(e){return this.check(Fa(Zs,e))},ipv4(e){return this.check(Ia(Qs,e))},ipv6(e){return this.check(La($s,e))},cidrv4(e){return this.check(Ra(ec,e))},cidrv6(e){return this.check(za(tc,e))},e164(e){return this.check(Ha(ic,e))},datetime(e){return this.check(Wa(Ls,e))},date(e){return this.check(Ga(Rs,e))},time(e){return this.check(Ka(zs,e))},duration(e){return this.check(qa(Bs,e))}});function Is(e){return xa(Fs,e)}var $=H(`ZodStringFormat`,(e,t)=>{q.init(e,t),Ps.init(e,t)}),Ls=H(`ZodISODateTime`,(e,t)=>{qr.init(e,t),$.init(e,t)}),Rs=H(`ZodISODate`,(e,t)=>{Jr.init(e,t),$.init(e,t)}),zs=H(`ZodISOTime`,(e,t)=>{Yr.init(e,t),$.init(e,t)}),Bs=H(`ZodISODuration`,(e,t)=>{Xr.init(e,t),$.init(e,t)}),Vs=H(`ZodEmail`,(e,t)=>{jr.init(e,t),$.init(e,t)}),Hs=H(`ZodGUID`,(e,t)=>{kr.init(e,t),$.init(e,t)}),Us=H(`ZodUUID`,(e,t)=>{Ar.init(e,t),$.init(e,t)}),Ws=H(`ZodURL`,(e,t)=>{zr.init(e,t),$.init(e,t)}),Gs=H(`ZodEmoji`,(e,t)=>{Br.init(e,t),$.init(e,t)}),Ks=H(`ZodNanoID`,(e,t)=>{Vr.init(e,t),$.init(e,t)}),qs=H(`ZodCUID`,(e,t)=>{Hr.init(e,t),$.init(e,t)}),Js=H(`ZodCUID2`,(e,t)=>{Ur.init(e,t),$.init(e,t)}),Ys=H(`ZodULID`,(e,t)=>{Wr.init(e,t),$.init(e,t)}),Xs=H(`ZodXID`,(e,t)=>{Gr.init(e,t),$.init(e,t)}),Zs=H(`ZodKSUID`,(e,t)=>{Kr.init(e,t),$.init(e,t)}),Qs=H(`ZodIPv4`,(e,t)=>{Zr.init(e,t),$.init(e,t)}),$s=H(`ZodIPv6`,(e,t)=>{ei.init(e,t),$.init(e,t)}),ec=H(`ZodCIDRv4`,(e,t)=>{ti.init(e,t),$.init(e,t)}),tc=H(`ZodCIDRv6`,(e,t)=>{ri.init(e,t),$.init(e,t)}),nc=H(`ZodBase64`,(e,t)=>{oi.init(e,t),$.init(e,t)}),rc=H(`ZodBase64URL`,(e,t)=>{li.init(e,t),$.init(e,t)}),ic=H(`ZodE164`,(e,t)=>{ui.init(e,t),$.init(e,t)}),ac=H(`ZodJWT`,(e,t)=>{fi.init(e,t),$.init(e,t)}),oc=H(`ZodNumber`,(e,t)=>{pi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>Qo(e,t,n,r),e.isFinite=!0},zt({minValue:e=>{let{minimum:t,exclusiveMinimum:n}=X(e);return Math.max(t??-1/0,n??-1/0)},maxValue:e=>{let{maximum:t,exclusiveMaximum:n}=X(e);return Math.min(t??1/0,n??1/0)},isInt:e=>{let{isInt:t,multipleOf:n}=X(e);return!!t||!!n?.some(Number.isSafeInteger)},format:e=>X(e).format??null},{gt(e,t){return this.check(eo(e,t))},gte(e,t){return this.check(to(e,t))},min(e,t){return this.check(to(e,t))},lt(e,t){return this.check(Qa(e,t))},lte(e,t){return this.check($a(e,t))},max(e,t){return this.check($a(e,t))},int(e){return this.check(lc(e))},safe(e){return this.check(lc(e))},positive(e){return this.check(eo(0,e))},nonnegative(e){return this.check(to(0,e))},negative(e){return this.check(Qa(0,e))},nonpositive(e){return this.check($a(0,e))},multipleOf(e,t){return this.check(no(e,t))},step(e,t){return this.check(no(e,t))},finite(){return this}}));function sc(e){return Ja(oc,e)}var cc=H(`ZodNumberFormat`,(e,t)=>{mi.init(e,t),oc.init(e,t)});function lc(e){return Ya(cc,e)}var uc=H(`ZodUnknown`,(e,t)=>{hi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(e,t,n)=>void 0});function dc(){return Xa(uc)}var fc=H(`ZodNever`,(e,t)=>{gi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>$o(e,t,n,r)});function pc(e){return Za(fc,e)}var mc=H(`ZodArray`,(e,t)=>{Ns(),bi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>rs(e,t,n,r),e.element=t.element},{min(e,t){return this.check(io(e,t))},nonempty(e){return this.check(io(1,e))},max(e,t){return this.check(ro(e,t))},length(e,t){return this.check(ao(e,t))},unwrap(){return this.element}});function hc(e,t){return yo(mc,e,t)}var gc=H(`ZodObject`,(e,t)=>{Ns(),Ei.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>as(e,t,n,r),Gt(e,`shape`,e=>e._zod.def.shape,!1)},{keyof(){return Cc(Object.keys(this._zod.def.shape))},catchall(e){return this.clone(I(this._zod.def,{catchall:e}))},passthrough(){return this.clone(I(this._zod.def,{catchall:dc()}))},loose(){return this.clone(I(this._zod.def,{catchall:dc()}))},strict(){return this.clone(I(this._zod.def,{catchall:pc()}))},strip(){return this.clone(I(this._zod.def,{catchall:void 0}))},extend(e){return bt(this,e)},safeExtend(e){return St(this,e)},merge(e){return Ct(this,e)},pick(e){return _t(this,e)},omit(e){return yt(this,e)},partial(...e){return wt(Ec,this,e[0])},exactPartial(...e){return wt(Oc,this,e[0],`exactPartial`)},required(...e){return Tt(Ic,this,e[0])}});function _c(e,t){return new gc({type:`object`,shape:e??{},...R(t)})}var vc=H(`ZodUnion`,(e,t)=>{Oi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>os(e,t,n,r),e.options=t.options});function yc(e,t){return new vc({type:`union`,options:e,...R(t)})}var bc=H(`ZodIntersection`,(e,t)=>{ki.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>ss(e,t,n,r)});function xc(e,t){return new bc({type:`intersection`,left:e,right:t})}var Sc=H(`ZodEnum`,(e,t)=>{Mi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>es(e,t,n,r),e.enum=t.entries,e.options=[...e._zod.values];let n=new Set(Object.keys(t.entries));e.extract=(e,r)=>{let i={};for(let r of e)if(n.has(r))i[r]=t.entries[r];else throw Error(`Key ${r} not found in enum`);return new Sc({...t,checks:[],...R(r),entries:i})},e.exclude=(e,r)=>{let i={...t.entries};for(let t of e)if(n.has(t))delete i[t];else throw Error(`Key ${t} not found in enum`);return new Sc({...t,checks:[],...R(r),entries:i})}});function Cc(e,t){return new Sc({type:`enum`,entries:Array.isArray(e)?Object.fromEntries(e.map(e=>[e,e])):e,...R(t)})}var wc=H(`ZodTransform`,(e,t)=>{Ns(),Ni.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>ns(e,t,n,r),e._zod.parse=(n,r)=>{if(r.direction===`backward`)throw new $t(e.constructor.name);n.addIssue=r=>{if(typeof r==`string`)n.issues.push(It(r,n.value,t));else{let t=r;t.fatal&&(t.continue=!1),t.code??=`custom`,`input`in t||(t.input=n.value),t.inst??=e,n.issues.push(It(t))}};let i=t.transform(n.value,n);return i instanceof Promise?i.then(e=>(n.value=e,n)):(n.value=i,n)}});function Tc(e){return new wc({type:`transform`,transform:e})}var Ec=H(`ZodOptional`,(e,t)=>{Fi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>_s(e,t,n,r),e.unwrap=()=>e._zod.def.innerType});function Dc(e){return new Ec({type:`optional`,innerType:e})}var Oc=H(`ZodExactOptional`,(e,t)=>{Ii.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>_s(e,t,n,r),e.unwrap=()=>e._zod.def.innerType});function kc(e){return new Oc({type:`optional`,innerType:e})}var Ac=H(`ZodNullable`,(e,t)=>{Li.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>cs(e,t,n,r),e.unwrap=()=>e._zod.def.innerType});function jc(e){return new Ac({type:`nullable`,innerType:e})}var Mc=H(`ZodDefault`,(e,t)=>{Ri.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>fs(e,t,n,r),e.unwrap=()=>e._zod.def.innerType,e.removeDefault=e.unwrap});function Nc(e,t){return new Mc({type:`default`,innerType:e,get defaultValue(){return typeof t==`function`?t():ut(t)}})}var Pc=H(`ZodPrefault`,(e,t)=>{Bi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>ps(e,t,n,r),e.unwrap=()=>e._zod.def.innerType});function Fc(e,t){return new Pc({type:`prefault`,innerType:e,get defaultValue(){return typeof t==`function`?t():ut(t)}})}var Ic=H(`ZodNonOptional`,(e,t)=>{Vi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>ls(e,t,n,r),e.unwrap=()=>e._zod.def.innerType});function Lc(e,t){return new Ic({type:`nonoptional`,innerType:e,...R(t)})}var Rc=H(`ZodCatch`,(e,t)=>{Wi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>ms(e,t,n,r),e.unwrap=()=>e._zod.def.innerType,e.removeCatch=e.unwrap});function zc(e,t){return new Rc({type:`catch`,innerType:e,catchValue:typeof t==`function`?t:qt(t)})}var Bc=H(`ZodPipe`,(e,t)=>{Gi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>hs(e,t,n,r),e.in=t.in,e.out=t.out});function Vc(e,t){return new Bc({type:`pipe`,in:e,out:t})}var Hc=H(`ZodReadonly`,(e,t)=>{qi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>gs(e,t,n,r),e.unwrap=()=>e._zod.def.innerType});function Uc(e){return new Hc({type:`readonly`,innerType:e})}var Wc=H(`ZodCustom`,(e,t)=>{Yi.init(e,t),Q.init(e,t),e._zod.processJSONSchema=(t,n,r)=>ts(e,t,n,r)});function Gc(e,t={}){return bo(Wc,e,t)}function Kc(e,t){return xo(e,t)}var qc=200,Jc=/^---\r?\n([\s\S]*?)\r?\n---\r?\n?/,Yc=_c({title:Is().min(1),date:Is().min(1),updated:Is().min(1).optional(),hero:Is().min(1),description:Is().min(1).optional(),author:_c({name:Is().min(1),avatar:Is().min(1).optional()}).optional(),readingTime:sc().int().positive().optional()}),Xc=Object.assign({"../../../web/public/blog/almost-pmf-and-a-roadmap-update/post.md":e,"../../../web/public/blog/ginormous-june/post.md":t,"../../../web/public/blog/how-loyal-uses-nillion-for-user-owned-data-storage/post.md":n,"../../../web/public/blog/how-to-control-the-future/post.md":r,"../../../web/public/blog/july-no-kings-or-gods-just-dogs/post.md":i,"../../../web/public/blog/loyal-for-seeker-but-why/post.md":a,"../../../web/public/blog/loyal-is-coming-to-solana-seeker/post.md":o,"../../../web/public/blog/loyal-is-live-on-solana-seeker/post.md":s,"../../../web/public/blog/markets-down-the-lower-risk-play/post.md":c,"../../../web/public/blog/may-was-good-to-us/post.md":l,"../../../web/public/blog/monthly-updates-telegram-agent-and-gtm/post.md":u,"../../../web/public/blog/open-source-and-e2e-encryption-wont-keep-you-safe/post.md":d,"../../../web/public/blog/passive-rewards-from-stables-you-already-own/post.md":f,"../../../web/public/blog/regain-control-of-your-digital-soul/post.md":p,"../../../web/public/blog/smart-accounts-guardrails-for-ai-agents/post.md":m,"../../../web/public/blog/solana-transactions-via-telegram-handle-are-live/post.md":h,"../../../web/public/blog/two-types-of-people-in-the-ai-era/post.md":g,"../../../web/public/blog/week-10-at-loyal/post.md":_,"../../../web/public/blog/week-14-at-loyal/post.md":v,"../../../web/public/blog/week-15-at-loyal/post.md":y,"../../../web/public/blog/week-16-at-loyal/post.md":b,"../../../web/public/blog/week-17-at-loyal/post.md":x,"../../../web/public/blog/week-18-at-loyal/post.md":S,"../../../web/public/blog/week-19-at-loyal/post.md":C,"../../../web/public/blog/week-20-at-loyal/post.md":ee,"../../../web/public/blog/week-21-at-loyal/post.md":te,"../../../web/public/blog/week-3-at-loyal/post.md":ne,"../../../web/public/blog/week-5-at-loyal/post.md":re,"../../../web/public/blog/week-7-at-loyal/post.md":ie,"../../../web/public/blog/week-9-at-loyal/post.md":ae}),Zc=`post.md`;function Qc(e){let t=e.match(Jc);return t?{data:Ue.load(t[1],{schema:Ue.JSON_SCHEMA})??{},content:e.slice(t[0].length)}:{data:{},content:e}}function $c(e){let t=e.trim().split(/\s+/).filter(Boolean).length;return Math.max(1,Math.round(t/qc))}function el(e,t){return/^(https?:)?\/\//.test(t)||t.startsWith(`/`)?t:`/blog/${e}/${t.replace(/^\.\//,``)}`}function tl(e,t){let n=/\/blog\/([^/]+)\/post\.md$/.exec(e)?.[1];if(!n)return null;let{data:r,content:i}=Qc(t),a=Yc.safeParse(r);if(!a.success)throw Error(`Invalid frontmatter in blog/${n}/${Zc}: ${a.error.message}`);let o=a.data;return{slug:n,title:o.title,date:o.date,updated:o.updated,hero:el(n,o.hero),description:o.description,author:o.author?{name:o.author.name,avatar:o.author.avatar?el(n,o.author.avatar):void 0}:void 0,readingTime:o.readingTime??$c(i),content:i}}var nl=Object.entries(Xc).map(([e,t])=>tl(e,t)).filter(e=>e!==null).sort((e,t)=>t.date.localeCompare(e.date));export{nl as allPosts};