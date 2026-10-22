import{s as e}from"./rolldown-runtime-Bjp52BOI.js";import{t}from"./react-BPZ7Dl3u.js";import{t as n}from"./jsx-runtime-CndCZlyt.js";import{$ as r,B as i,K as a,M as o,at as s}from"./esm-BJWRn8OP.js";import"./context-DEETNFc9-CiNZw6c8.js";import{r as c}from"./Error-C0yEZpNv.js";import{E as l,Ht as u,M as d,N as ee,b as te}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{i as f,n as p,t as m}from"./use-deposit-address-BxngCd-N-DrsriehQ.js";import{r as ne}from"./eventemitter3-DqDdb4wS.js";import{n as h,t as re}from"./chevron-down-7zECc70b.js";import{t as ie}from"./modal-context-BAV3sQpp-k4X-kX2b.js";import{a as ae,l as oe,n as se,o as ce}from"./floating-ui.react-dom-Ba-aju-H.js";import{a as le,c as ue,d as de,f as fe,i as g,o as _,r as pe,s as me,t as he,u as ge}from"./floating-ui.react-KGW9AcFf.js";import{t as _e}from"./browser-CVjgFsl-.js";import{t as v}from"./createLucideIcon-BL8LZspr.js";import{t as y}from"./check-CMoNoGI-.js";import{t as ve}from"./hourglass-D8OJeMgQ.js";import{t as ye}from"./info-C_MDT-rX.js";import{_ as b,a as x,c as S,d as be,f as C,g as w,h as T,i as E,l as D,m as O,n as k,o as A,p as xe,r as j,s as M,t as N,u as Se,v as P}from"./styles-DgsHCTKB-DTE6RwN7.js";import{t as F}from"./triangle-alert-D5l2WO2i.js";import{t as Ce}from"./CopyableText-ChtfBWx4-BJRAdFbm.js";import{l as we}from"./ModalHeader-CAqKnddp-BVsHJbWY.js";import{t as Te}from"./QrCode-BV7KfzAf-B8-EO96f.js";import{r as I}from"./styles-DVyDvTdj-ipH1ixTV.js";import{t as L}from"./ScreenLayout-Yoa2TSpi-GNprbNng.js";function R(e){return e.startsWith(`eip155:`)?`ethereum`:e.startsWith(`solana:`)?`solana`:e.startsWith(`bip122:`)?`bitcoin-segwit`:e.startsWith(`tron:`)?`tron`:void 0}async function z(e){let{user:t}=await e.privy.user.get();if(!t)return{ok:!1,error:`NOT_AUTHENTICATED`};let n=function(e,t){let n=R(e);if(!n)return;let r=t.linked_accounts.find((e=>e.type===`wallet`&&e.chain_type===n&&`address`in e&&e.address));return r&&`address`in r?r.address:void 0}(e.caip2,t);if(n)return{ok:!0,address:n};let r=R(e.caip2);if(!r)return{ok:!1,error:`UNSUPPORTED_CHAIN`};try{let t=await e.privy.fetchPrivyRoute(o,{body:{chain_type:r}});return await e.onWalletCreated?.(),{ok:!0,address:t.address}}catch{return{ok:!1,error:`REFUND_WALLET_CREATION_FAILED`}}}async function Ee(e){let{user:t}=await e.privy.user.get();if(!t)throw Error(`NOT_AUTHENTICATED`);let n=e.refundAddress;if(!n){let t=await z({privy:e.privy,caip2:e.sourceChain,onWalletCreated:e.onWalletCreated});if(!t.ok)throw Error(t.error);n=t.address}return await e.privy.fetchPrivyRoute(i,{body:{source_chain:e.sourceChain,source_currency:e.sourceCurrency,destination_chain:e.destinationChain,destination_currency:e.destinationCurrency,destination_address:e.destinationAddress,refund_address:n,...e.slippageBps==null?{}:{slippage_bps:e.slippageBps}}})}function B(e,t){return Math.ceil(t/e)}function V(e){return e.status===`success`?e.result?{status:`success`,order:e.result}:{status:`timeout`}:e.status===`aborted`?{status:`aborted`,error:e.error}:{status:`timeout`,error:e.error}}async function De(e){return await e.privy.fetchPrivyRoute(s,{params:{order_id:e.orderId}})}async function Oe(e){let t=e.pollIntervalMs??2e3,n=e.timeoutMs??18e5,i=e.signal??new AbortController().signal;return V(await h({operation:async()=>{let t=await e.privy.fetchPrivyRoute(r,{params:{deposit_address_id:e.depositAddressId},query:{after:e.quoteCreatedAt}});if(t.order)return await e.privy.fetchPrivyRoute(s,{params:{order_id:t.order.id}})},until:e=>e!==void 0,delay:t,interval:t,attempts:B(t,n),signal:i}))}async function ke(e){let t=e.pollIntervalMs??2e3,n=e.timeoutMs??18e5,r=e.signal??new AbortController().signal;return V(await h({operation:()=>e.privy.fetchPrivyRoute(s,{params:{order_id:e.orderId}}),until:e=>e.status!==`executing`,delay:t,interval:t,attempts:B(t,n),signal:r}))}async function Ae(e){let t=await e.fetchPrivyRoute(a,{});return{currencies:t.currencies,chains:t.chains}}var H=Object.freeze({__proto__:null,generateDepositAddress:Ee,getConfig:Ae,getDeposit:De,resolveRefundAddress:z,waitForCompletion:ke,waitForDeposit:Oe}),je=v(`chevron-up`,[[`path`,{d:`m18 15-6-6-6 6`,key:`153udz`}]]),Me=v(`undo-2`,[[`path`,{d:`M9 14 4 9l5-5`,key:`102s5s`}],[`path`,{d:`M4 9h10.5a5.5 5.5 0 0 1 5.5 5.5a5.5 5.5 0 0 1-5.5 5.5H11`,key:`f3b9sd`}]]),U=n(),W=e(t(),1);d(),_e(),ne();var Ne=class extends W.Component{static getDerivedStateFromError(){return{hasError:!0}}componentDidCatch(e,t){this.props.onError(e)}componentDidUpdate(e){e.resetKey!==this.props.resetKey&&this.state.hasError&&this.setState({hasError:!1})}render(){return this.state.hasError?null:this.props.children}constructor(...e){super(...e),this.state={hasError:!1}}};function Pe(e,t,n){let r=Number(e);return!Number.isFinite(r)||r===0?`1 ${t} ≈ ${e} ${n}`:r>=.01?`1 ${t} ≈ ${G(r)} ${n}`:`${G(1/r)} ${t} ≈ 1 ${n}`}function G(e){return e>=1e3?new Intl.NumberFormat(`en-US`,{maximumFractionDigits:0}).format(Math.round(e)):e>=100?new Intl.NumberFormat(`en-US`,{maximumFractionDigits:1}).format(e):e>=1?new Intl.NumberFormat(`en-US`,{maximumFractionDigits:2}).format(e):new Intl.NumberFormat(`en-US`,{maximumFractionDigits:4}).format(e)}function K(e,t){let n=Number(e);if(!Number.isFinite(n)||n===0)return e;let r=t==null?n:n/10**t;return r>=1e3?new Intl.NumberFormat(`en-US`,{maximumFractionDigits:2}).format(r):r>=1?new Intl.NumberFormat(`en-US`,{maximumFractionDigits:4}).format(r):r>=1e-4?new Intl.NumberFormat(`en-US`,{maximumFractionDigits:6}).format(r):new Intl.NumberFormat(`en-US`,{maximumSignificantDigits:4}).format(r)}function q({address:e,caip2:t,config:n}){for(let r of n.currencies){let n=r.chains.find((n=>n.caip2===t&&n.address.toLowerCase()===e.toLowerCase()));if(n)return{symbol:r.symbol.toUpperCase(),decimals:n.decimals}}return{symbol:e,decimals:void 0}}function J(e,t){return t[e]?.displayName??e}function Y(e,t){if(!e.chains[t.destinationChain])return`Unsupported destination chain: "${t.destinationChain}". Check that the chain is in CAIP-2 format (e.g. "eip155:8453") and is supported for deposit addresses.`;let n=t.destinationCurrency.toLowerCase();return e.currencies.some((e=>e.chains.some((e=>e.caip2===t.destinationChain&&e.address.toLowerCase()===n))))?null:`Unsupported destination currency "${t.destinationCurrency}" on chain "${t.destinationChain}". Check that this token address is supported on the specified chain.`}var Fe=new Set([`ROUTE_UNAVAILABLE`,`UNEXPECTED_STATE`,`TIMEOUT_WAITING_FOR_NEXT_ORDER`,`TIMEOUT_ORDER_COMPLETION`,`DEPOSIT_FAILED`,`DEPOSIT_REFUNDED`,`USER_EXITED`,`AMOUNT_TOO_LOW`,`INSUFFICIENT_LIQUIDITY`,`UNSUPPORTED_CHAIN`,`UNSUPPORTED_CURRENCY`,`UNSUPPORTED_ROUTE`,`NO_SWAP_ROUTES_FOUND`,`NO_INTERNAL_SWAP_ROUTES_FOUND`,`NO_QUOTES`,`SANCTIONED_WALLET_ADDRESS`,`REFUND_WALLET_CREATION_FAILED`,`DEPOSIT_ADDRESSES_NOT_ENABLED`,`NOT_AUTHENTICATED`]);function Ie(e){return Fe.has(e)}function X(e){return Ie(e)?e:`UNKNOWN_ERROR`}function Z(){let{params:e,setModalState:t}=f(),{privy:n}=u(),r=function(){let{privy:e,refreshSessionAndUser:t}=u();return(0,W.useCallback)(((n,r)=>r?Promise.resolve({ok:!0,address:r}):H.resolveRefundAddress({privy:e,caip2:n,onWalletCreated:t})),[e,t])}(),[a,o]=(0,W.useState)(!1);return{fetchQuote:(0,W.useCallback)((async(a,s,l)=>{if(e){o(!0);try{let o=await r(a.caip2,e.refundAddress);if(!o.ok)return void t({step:`error`,code:X(o.error)});let c=await n.fetchPrivyRoute(i,{body:{source_chain:a.caip2,source_currency:a.currencyAddress,destination_chain:e.destinationChain,destination_currency:e.destinationCurrency,destination_address:e.destinationAddress,refund_address:o.address,...e.slippageBps==null?{}:{slippage_bps:e.slippageBps}}});t({step:`address`,selectedCurrency:s,selectedChain:a,availableChains:l,quote:c})}catch(e){let n=e instanceof Error?e:Error(String(e)),r=`status`in n&&typeof n.status==`number`?n.status:void 0;t({step:`error`,code:n instanceof c&&n.code===`feature_not_enabled`?`DEPOSIT_ADDRESSES_NOT_ENABLED`:r&&r>=500?`UNKNOWN_ERROR`:X(n.message),message:n.message})}finally{o(!1)}}}),[e,n,r,t]),isFetching:a}}function Q(e,t){switch(e.status){case`completed`:return t({step:`complete`,order:e});case`refunded`:return t({step:`refunded`,order:e});case`failed`:return t({step:`failed`,order:e});case`executing`:return t({step:`processing`,order:e});default:return}}var Le=({sourceAmount:e,sourceSymbol:t,sourceChainName:n,sourceDecimals:r,destinationAmount:i,destSymbol:a,destChainName:o,destDecimals:s,onClose:c})=>(0,U.jsx)(S,{icon:y,iconVariant:`success`,title:`Transfer complete`,subtitle:i?`Received ${K(e,r)} ${t} on ${n} and converted it to ${K(i,s)} ${a} on ${o}. Funds are available to use.`:`Your ${t} has been received and is now available in your wallet.`,showClose:!0,onClose:c,primaryCta:{label:`Done`,onClick:c},watermark:!1});function Re(){let{state:e,configData:t,close:n}=p(`complete`),{order:r}=e,{sourceSymbol:i,sourceChainName:a,sourceDecimals:o,destSymbol:s,destChainName:c,destDecimals:l}=(0,W.useMemo)((()=>{let e=q({address:r.source_currency,caip2:r.source_chain,config:t}),n=q({address:r.destination_currency,caip2:r.destination_chain,config:t});return{sourceSymbol:e.symbol,sourceChainName:J(r.source_chain,t.chains),sourceDecimals:e.decimals,destSymbol:n.symbol,destChainName:J(r.destination_chain,t.chains),destDecimals:n.decimals}}),[r,t]);return(0,U.jsx)(Le,{sourceAmount:r.source_amount,sourceSymbol:i,sourceChainName:a,sourceDecimals:o,destinationAmount:r.destination_amount,destSymbol:s,destChainName:c,destDecimals:l,onClose:n})}function ze(){let{modalState:e,setModalState:t,config:n,retryConfig:r,close:i}=f();if(e.step!==`error`)throw Error(`UNEXPECTED_STATE`);let{code:a}=e,{title:o,subtitle:s,detail:c,iconVariant:l}=(e=>{switch(e){case`AMOUNT_TOO_LOW`:return{title:`Amount too low`,subtitle:`The deposit amount is below the minimum for this route.`,detail:`Try a larger amount or a different token.`,iconVariant:`warning`};case`INSUFFICIENT_LIQUIDITY`:return{title:`Insufficient liquidity`,subtitle:`There isn't enough liquidity for this route right now.`,detail:`Try a smaller amount or a different network.`,iconVariant:`warning`};case`UNSUPPORTED_CHAIN`:return{title:`Unsupported chain`,subtitle:`Deposits from this chain type aren't supported yet. Try a different network.`,iconVariant:`warning`};case`UNSUPPORTED_CURRENCY`:case`UNSUPPORTED_ROUTE`:case`ROUTE_UNAVAILABLE`:case`NO_SWAP_ROUTES_FOUND`:case`NO_INTERNAL_SWAP_ROUTES_FOUND`:case`NO_QUOTES`:return{title:`Route not available`,subtitle:`This deposit route isn't supported right now. Try a different token or network.`,iconVariant:`warning`};case`SANCTIONED_WALLET_ADDRESS`:return{title:`Address restricted`,subtitle:`This address cannot be used for deposits due to compliance restrictions.`,iconVariant:`warning`};case`REFUND_WALLET_CREATION_FAILED`:return{title:`Unable to set up refund address`,subtitle:`We couldn't create a wallet to receive refunds on this chain. Please try again or select a different network.`,iconVariant:`warning`};case`DEPOSIT_ADDRESSES_NOT_ENABLED`:return{title:`Not enabled`,subtitle:`Deposit addresses are not enabled for this app.`,iconVariant:`warning`};case`NOT_AUTHENTICATED`:return{title:`Not signed in`,subtitle:`Please sign in to continue with your deposit.`,iconVariant:`warning`};case`TIMEOUT_WAITING_FOR_NEXT_ORDER`:case`TIMEOUT_ORDER_COMPLETION`:return{title:`Taking longer than expected`,subtitle:`Your funds are safe. The deposit is still being processed — check back later.`,iconVariant:`subtle`};default:return{title:`Something went wrong`,subtitle:`We couldn't complete your request. Please try again.`,iconVariant:`subtle`}}})(a),[u,d]=(0,W.useState)(!1);return(0,U.jsx)(S,{icon:F,iconVariant:l,title:o,subtitle:c?`${s} ${c}`:s,showClose:!0,onClose:i,primaryCta:{label:`Try again`,onClick:async()=>{if(n.status!==`ready`){d(!0);try{await r(),t({step:`token`})}catch{d(!1)}}else t({step:`token`})},loading:u},watermark:!0})}function Be(){let{state:e,close:t}=p(`failed`),{order:n}=e;return(0,U.jsx)(L,{icon:F,iconVariant:`error`,title:`Transfer failed`,subtitle:`Something went wrong processing your transfer.`,showClose:!0,onClose:t,primaryCta:{label:`Done`,onClick:t},secondaryCta:{label:`Learn about manual recovery`,onClick:()=>window.open(`https://docs.privy.io`,`_blank`,`noopener,noreferrer`)},watermark:!0,children:(0,U.jsxs)(Ve,{href:n.tracking_url,target:`_blank`,rel:`noopener noreferrer`,children:[`Reference: `,n.provider_request_id]})})}var Ve=l.a`
  text-align: center;
  font-size: 0.75rem;
  opacity: 0.7;
  text-decoration: underline;
  cursor: pointer;
  color: var(--privy-color-foreground-3);
`;function He(){let{close:e,setModalState:t,config:n,params:r,onBack:i}=f(),[a,o]=(0,W.useState)(!1);return(0,W.useEffect)((()=>{if(a&&r){if(n.status===`ready`){let e=Y(n.data,r);t(e?{step:`error`,code:`ROUTE_UNAVAILABLE`,message:e}:{step:`token`})}n.status===`error`&&t({step:`error`,code:`ROUTE_UNAVAILABLE`})}}),[a,n,r,t]),(0,U.jsx)(S,{icon:P,iconVariant:`subtle`,title:`Add funds`,subtitle:`Top up your account by sending crypto from any wallet. Conversion and routing handled by Relay.`,showClose:!0,onClose:e,showBack:!!i,onBack:i,primaryCta:{label:`Continue`,onClick:()=>{if(n.status===`ready`&&r){let e=Y(n.data,r);t(e?{step:`error`,code:`ROUTE_UNAVAILABLE`,message:e}:{step:`token`})}else n.status===`error`?t({step:`error`,code:`ROUTE_UNAVAILABLE`}):o(!0)},loading:a&&n.status===`loading`,loadingText:null},watermark:!0})}function Ue(){let{state:e,setModalState:t,close:n}=p(`network`),[r,i]=(0,W.useState)(-1),{availableChains:a}=e,{confirm:o,isFetching:s}=function(){let e=m(),{params:t}=f(),{fetchQuote:n,isFetching:r}=Z();return{confirm:(0,W.useCallback)((async r=>{if(!r||!t)return;let i=e?.modalState;i&&i.step===`network`&&await n(r,i.selectedCurrency,i.availableChains)}),[t,e,n]),isFetching:r}}();return(0,U.jsx)(L,{title:`Select network`,eyebrow:(0,U.jsxs)(`span`,{style:{display:`flex`,alignItems:`center`,gap:`0.375rem`},children:[(0,U.jsx)(`img`,{src:e.selectedCurrency.logoURI,alt:``,style:{width:`1rem`,height:`1rem`,borderRadius:`50%`}}),`Send `,e.selectedCurrency.symbol]}),showBack:!0,onBack:()=>t({step:`token`}),showClose:!0,onClose:n,watermark:!0,children:(0,U.jsx)(I,{style:{marginTop:`1rem`,height:`22rem`},$colorScheme:`light`,children:a.map(((e,t)=>(0,U.jsxs)(E,{$selected:r===t,disabled:s,onClick:()=>{i(t),o(e)},children:[(0,U.jsx)(O,{src:e.iconUrl,alt:e.displayName}),(0,U.jsx)(D,{children:e.displayName}),s&&t===r&&(0,U.jsx)(b,{})]},e.caip2)))})})}var We=({trackingUrl:e,onClose:t})=>(0,U.jsx)(L,{icon:ve,iconVariant:`subtle`,title:`Transfer in progress`,subtitle:`Your deposit was received and the transfer is now processing.`,showClose:!0,onClose:t,secondaryCta:{label:`View on block explorer ↗`,onClick:()=>window.open(e,`_blank`,`noopener,noreferrer`)},watermark:!1,children:(0,U.jsxs)(Se,{children:[(0,U.jsxs)(A,{children:[(0,U.jsx)(C,{$status:`done`,children:(0,U.jsx)(y,{size:14,color:`var(--privy-color-icon-success)`,strokeWidth:2})}),(0,U.jsx)(x,{children:`Deposit received`})]}),(0,U.jsx)(w,{}),(0,U.jsxs)(A,{children:[(0,U.jsx)(C,{$status:`active`,children:(0,U.jsx)(Ge,{})}),(0,U.jsx)(x,{children:`Bridging`})]}),(0,U.jsx)(w,{}),(0,U.jsxs)(A,{children:[(0,U.jsx)(C,{$status:`pending`}),(0,U.jsx)(x,{children:`Funds arrived`})]})]})}),Ge=l.span`
  width: 0.75rem;
  height: 0.75rem;
  border: 2px solid var(--privy-color-foreground-3);
  border-bottom-color: transparent;
  border-radius: 50%;
  display: inline-block;
  animation: spin 1s linear infinite;

  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
`;function Ke(){let{state:e,close:t}=p(`processing`);return function({orderId:e,enabled:t}){let{privy:n}=u(),{setModalState:r}=f();(0,W.useEffect)((()=>{let t=new AbortController;return H.waitForCompletion({privy:n,orderId:e,signal:t.signal}).then((e=>{t.signal.aborted||(e.status===`success`?Q(e.order,r):e.status===`timeout`&&r({step:`error`,code:`TIMEOUT_ORDER_COMPLETION`}))})),()=>{t.abort()}}),[t,e,n,r])}({orderId:e.order.id,enabled:!0}),(0,U.jsx)(We,{trackingUrl:e.order.tracking_url,onClose:t})}function qe(){let{state:e,close:t}=p(`refunded`),{order:n}=e;return(0,U.jsx)(S,{icon:Me,iconVariant:`subtle`,title:`Transfer refunded`,subtitle:`Your transfer was received, but the swap couldn't be completed. A refund has been started automatically.`,showClose:!0,onClose:t,primaryCta:{label:`Done`,onClick:t},secondaryCta:{label:`View transaction details`,onClick:()=>window.open(n.tracking_url,`_blank`,`noopener,noreferrer`)},watermark:!0})}function Je(){let{close:e,setModalState:t,config:n}=f(),{confirm:r,currencies:i,isFetching:a}=function(){let{config:e,setModalState:t}=f(),{fetchQuote:n,isFetching:r}=Z(),i=e.status===`ready`?e.data.currencies:[];return{confirm:(0,W.useCallback)((async r=>{if(e.status!==`ready`||!r)return;let i=function(e,t){return e.chains.map((e=>{let n=t.chains[e.caip2];return n?{caip2:e.caip2,displayName:n.displayName,iconUrl:n.iconUrl,vmType:n.vmType,currencyAddress:e.address,currencyDecimals:e.decimals}:null})).filter((e=>e!==null))}(r,e.data);if(i.length!==1)t({step:`network`,selectedCurrency:r,availableChains:i});else{let e=i[0];await n(e,r,i)}}),[e,n,t]),currencies:i,isFetching:r}}(),[o,s]=(0,W.useState)(-1);return(0,U.jsx)(L,{title:`Select token`,showBack:!0,onBack:()=>t({step:`intro`}),showClose:!0,onClose:e,watermark:!0,children:n.status===`error`?(0,U.jsx)(j,{children:(0,U.jsx)(be,{children:`Failed to load tokens`})}):n.status===`loading`?(0,U.jsx)(j,{children:(0,U.jsx)(te,{})}):(0,U.jsx)(I,{style:{marginTop:`1rem`,height:`22rem`},$colorScheme:`light`,children:i.map(((e,t)=>(0,U.jsxs)(E,{$selected:o===t,disabled:a,onClick:()=>{s(t),r(e)},children:[(0,U.jsx)(N,{src:e.logoURI,alt:e.symbol}),(0,U.jsx)(D,{children:e.name}),a&&t===o?(0,U.jsx)(b,{}):(0,U.jsx)(xe,{children:e.symbol})]},e.symbol)))})})}function Ye({address:e,onClick:t}){let[n,r]=(0,W.useState)(!1);return(0,U.jsx)(U.Fragment,{children:n?(0,U.jsx)(Xe,{onClick:()=>r(!1),style:{marginTop:`1.5rem`},children:(0,U.jsx)(Te,{url:e,size:312,hideLogo:!0})}):(0,U.jsxs)(Ze,{title:`Click to copy address`,onClick:t,style:{marginTop:`1.5rem`},children:[(0,U.jsxs)(Qe,{children:[(0,U.jsx)($e,{children:`Deposit address`}),(0,U.jsx)(et,{children:e})]}),(0,U.jsx)(tt,{children:(0,U.jsx)(nt,{type:`button`,onClick:e=>{e.stopPropagation(),r(!0)},children:(0,U.jsx)(P,{size:16,color:`var(--privy-color-icon-muted)`})})})]})})}var Xe=l.div`
  display: flex;
  justify-content: center;
  align-items: center;
  cursor: pointer;
  overflow: hidden;
`,Ze=l.div`
  display: flex;
  border-radius: var(--privy-border-radius-md);
  background: var(--privy-color-background-clicked, #f1f2f9);
  padding: 1rem;
  cursor: pointer;
  gap: 0.5rem;
`,Qe=l.div`
  flex: 1;
  min-width: 0;
  text-align: left;
`,$e=l.div`
  font-size: 0.75rem;
  color: var(--privy-color-icon-muted);
  line-height: 1rem;
  margin-bottom: 0.25rem;
`,et=l.div`
  word-break: break-all;
  font-size: 0.875rem;
  font-family: ui-monospace, monospace;
  font-weight: 500;
  line-height: 1.375rem;
  color: var(--privy-color-foreground);
`,tt=l.div`
  width: 1.5rem;
  flex-shrink: 0;
  display: flex;
  justify-content: center;
  padding-top: 0.25rem;
`,nt=l.button`
  && {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 1.5rem;
    height: 1.5rem;
    border: none;
    background: transparent;
    cursor: pointer;
    outline: none;
    box-shadow: none;
    border-radius: var(--privy-border-radius-xs);

    &:hover {
      background: var(--privy-color-background);
    }

    &:focus,
    &:focus-visible {
      outline: none;
      box-shadow: none;
    }
  }
`;function rt({quote:e,selectedCurrency:t,selectedChain:n,destinationSymbol:r}){let[i,a]=(0,W.useState)(!1),o=t.symbol.toUpperCase(),s=n.displayName,c=(0,W.useRef)(null);return(0,U.jsxs)(it,{children:[(0,U.jsxs)(at,{onClick:(0,W.useCallback)((()=>{let e=document.getElementById(`privy-modal-content`);e&&(c.current&&clearTimeout(c.current),e.style.transition=`none`,c.current=setTimeout((()=>{e.style.transition=``,c.current=null}),160)),a((e=>!e))}),[]),children:[(0,U.jsxs)(ot,{children:[t.logoURI&&(0,U.jsx)(N,{src:t.logoURI,alt:o,style:{width:`2rem`,height:`2rem`}}),n.iconUrl&&(0,U.jsx)(st,{src:n.iconUrl,alt:s})]}),(0,U.jsxs)(ct,{children:[(0,U.jsx)(lt,{children:`You send`}),(0,U.jsxs)(ut,{children:[o,` on `,s]})]}),(0,U.jsx)(dt,{children:(0,U.jsx)(i?je:re,{size:16})})]}),(0,U.jsx)(mt,{$expanded:i,children:(0,U.jsx)(ht,{children:(0,U.jsxs)(ft,{children:[e.indicative_rate&&(0,U.jsxs)(T,{children:[(0,U.jsx)(M,{children:`Conversion rate`}),(0,U.jsxs)(k,{style:{display:`flex`,alignItems:`center`,gap:`0.25rem`},children:[Pe(e.indicative_rate,o,r.toUpperCase()),(0,U.jsx)(gt,{content:`Estimated rate based on current market conditions. Final execution price may vary depending on transfer size and routing.`})]})]}),(0,U.jsxs)(T,{children:[(0,U.jsx)(M,{children:`Max slippage`}),(0,U.jsxs)(k,{children:[(e.slippage_bps/100).toFixed(1),`%`]})]}),(0,U.jsxs)(T,{children:[(0,U.jsx)(M,{children:`Refund address`}),(0,U.jsx)(k,{children:(0,U.jsx)(Ce,{value:e.refund_address,iconOnly:!0,iconSize:11,children:ee(e.refund_address,4,4)})})]})]})})}),(0,U.jsxs)($,{children:[(0,U.jsx)(F,{size:16,color:`var(--privy-color-icon-muted)`,style:{flexShrink:0}}),(0,U.jsxs)(pt,{children:[`Only send `,(0,U.jsx)(`strong`,{children:o}),` on `,(0,U.jsx)(`strong`,{children:s}),`. Other assets may be lost.`]})]})]})}var it=l.div`
  border-radius: var(--privy-border-radius-md);
  border: 1px solid var(--privy-color-foreground-4);
  overflow: hidden;
`,at=l.button`
  && {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.75rem 1rem;
    background: transparent;
    border: none;
    cursor: pointer;
    color: var(--privy-color-foreground);
    outline: none;
    box-shadow: none;

    &:focus,
    &:focus-visible {
      outline: none;
      box-shadow: none;
    }
  }
`,ot=l.span`
  position: relative;
  width: 2rem;
  height: 2rem;
  flex-shrink: 0;
`,st=l(O)`
  && {
    position: absolute;
    top: -0.125rem;
    right: -0.25rem;
    width: 0.75rem;
    height: 0.75rem;
    box-sizing: content-box;
    border: 1.5px solid #fff;
    background-color: #fff;
  }
`,ct=l.div`
  display: flex;
  flex-direction: column;
  align-items: flex-start;
`,lt=l.span`
  font-size: 0.75rem;
  color: var(--privy-color-foreground-3);
  line-height: 1rem;
`,ut=l.span`
  font-size: 0.875rem;
  font-weight: 500;
  line-height: 1.25rem;
`,dt=l.span`
  margin-left: auto;
  display: flex;
  align-items: center;
  justify-content: center;
  width: 1.5rem;
  height: 1.5rem;
  border-radius: var(--privy-border-radius-full);
  background-color: var(--privy-color-background-clicked, #f1f2f9);
  color: var(--privy-color-foreground-3);
`,ft=l.div`
  display: flex;
  flex-direction: column;
  padding: 0 1rem 0.75rem;

  & > * {
    padding: 0.5rem 0;
    border-bottom: 1px solid var(--privy-color-foreground-4);
  }

  & > *:last-child {
    border-bottom: none;
  }
`,$=l.div`
  display: flex;
  align-items: center;
  gap: 0.5rem;
  margin: 0 0.75rem 0.75rem;
  padding: 0.625rem 0.75rem;
  border-radius: var(--privy-border-radius-sm);
  background: #f8f9fc;
`,pt=l.span`
  font-size: 0.8125rem;
  line-height: 1.25rem;
  color: var(--privy-color-icon-muted);
  text-align: left;
`,mt=l.div`
  display: grid;
  grid-template-rows: ${({$expanded:e})=>e?`1fr`:`0fr`};
  transition: grid-template-rows 150ms ease-out;
`,ht=l.div`
  overflow: hidden;
`;function gt({content:e}){let[t,n]=(0,W.useState)(!1),{refs:r,floatingStyles:i,context:a}=_({open:t,onOpenChange:n,placement:`top`,whileElementsMounted:oe,middleware:[ae(6),se(),ce({padding:8})]}),o=ue(a,{move:!1,handleClose:pe()}),s=me(a),{getReferenceProps:c,getFloatingProps:l}=ge([o,s,g(a),le(a),de(a,{role:`tooltip`})]),{isMounted:u,styles:d}=fe(a,{duration:150});return(0,U.jsxs)(U.Fragment,{children:[(0,U.jsx)(`button`,{ref:r.setReference,type:`button`,"aria-label":`More information about conversion rate`,style:{display:`inline-flex`,alignItems:`center`,justifyContent:`center`,padding:0,border:`none`,background:`none`,color:`var(--privy-color-icon-muted)`,cursor:`pointer`},...c(),children:(0,U.jsx)(ye,{size:14})}),u&&(0,U.jsx)(he,{root:document.getElementById(`privy-modal-content`)??void 0,children:(0,U.jsx)(_t,{ref:r.setFloating,style:{...i,...d},...l(),children:e})})]})}var _t=l.div`
  max-width: 13rem;
  padding: 0.5rem 0.625rem;
  border-radius: var(--privy-border-radius-sm, 0.375rem);
  background: var(--privy-color-foreground);
  color: var(--privy-color-background);
  font-size: 0.6875rem;
  line-height: 1rem;
  font-weight: 400;
  text-align: left;
  z-index: 10;
`,vt=({quote:e,selectedCurrency:t,selectedChain:n,destinationSymbol:r,onBack:i,onClose:a})=>{let[o,s]=(0,W.useState)(!1),c=t?.symbol?.toUpperCase()??`funds`,l=n?.displayName??``,u=async()=>{o||(await navigator.clipboard.writeText(e.deposit_address),s(!0),setTimeout((()=>s(!1)),2e3))};return(0,U.jsxs)(L,{title:`Send ${c}${l?` on ${l}`:``}`,subtitle:`Send funds to the address below. Conversion and routing handled by Relay.`,showBack:!0,onBack:i,showClose:!0,onClose:a,watermark:!1,children:[(0,U.jsx)(rt,{quote:e,selectedCurrency:t,selectedChain:n,destinationSymbol:r}),(0,U.jsx)(Ye,{address:e.deposit_address,onClick:u}),(0,U.jsx)(we,{style:{marginTop:`1rem`,marginBottom:`0.5rem`,...o?{backgroundColor:`var(--privy-color-icon-success)`,borderColor:`var(--privy-color-icon-success)`}:{}},onClick:u,children:o?(0,U.jsxs)(U.Fragment,{children:[`Copied `,(0,U.jsx)(y,{size:16,style:{marginLeft:`0.25rem`}})]}):`Copy address`}),(0,U.jsx)(yt,{children:`Routing and bridging are handled by Relay. Privy does not control execution timing, liquidity, or transaction outcomes.`})]})},yt=l.p`
  && {
    margin: 0.5rem 0 0;
    font-size: 0.6875rem;
    line-height: 1.125rem;
    color: var(--privy-color-icon-muted);
    text-align: center;
  }
`;function bt(){let{state:e,configData:t,setModalState:n,close:r,params:i}=p(`address`),{quote:a,selectedCurrency:o,selectedChain:s,availableChains:c}=e;return function({depositAddressId:e,enabled:t,quoteCreatedAt:n}){let{privy:r}=u(),{setModalState:i}=f();(0,W.useEffect)((()=>{if(!e)return;let t=new AbortController;return H.waitForDeposit({privy:r,depositAddressId:e,quoteCreatedAt:n,signal:t.signal}).then((e=>{t.signal.aborted||(e.status===`success`?Q(e.order,i):e.status===`timeout`&&i({step:`error`,code:`TIMEOUT_WAITING_FOR_NEXT_ORDER`}))})),()=>{t.abort()}}),[t,e,r,n,i])}({depositAddressId:a.id,enabled:!0,quoteCreatedAt:a.created_at}),(0,U.jsx)(vt,{quote:a,selectedCurrency:o,selectedChain:s,destinationSymbol:(0,W.useMemo)((()=>q({address:i.destinationCurrency,caip2:i.destinationChain,config:t}).symbol),[i,t]),onBack:()=>n({step:`network`,selectedCurrency:o,availableChains:c}),onClose:r})}function xt(){let{modalState:e,setModalState:t}=f();return(0,U.jsx)(Ne,{onError:e=>t({step:`error`,code:`UNEXPECTED_STATE`,message:e.message}),resetKey:e.step,children:(0,U.jsx)(St,{})})}function St(){let{modalState:e}=f();switch(e.step){case`intro`:return(0,U.jsx)(He,{});case`token`:return(0,U.jsx)(Je,{});case`network`:return(0,U.jsx)(Ue,{});case`address`:return(0,U.jsx)(bt,{});case`processing`:return(0,U.jsx)(Ke,{});case`complete`:return(0,U.jsx)(Re,{});case`refunded`:return(0,U.jsx)(qe,{});case`failed`:return(0,U.jsx)(Be,{});case`error`:return(0,U.jsx)(ze,{});default:return null}}var Ct={component:()=>{let{onUserCloseViaDialogOrKeybindRef:e}=ie(),t=m(),{close:n,config:r}=f();return(0,W.useEffect)((()=>{e.current=n}),[e,n]),(0,W.useEffect)((()=>{if(r.status===`ready`){for(let e of r.data.currencies)new Image().src=e.logoURI;for(let e of Object.values(r.data.chains))new Image().src=e.iconUrl}}),[r]),t?(0,U.jsx)(xt,{}):null}};export{Ct as default};