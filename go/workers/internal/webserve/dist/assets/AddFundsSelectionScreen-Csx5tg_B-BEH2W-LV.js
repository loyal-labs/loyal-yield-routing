import{s as e}from"./rolldown-runtime-Bjp52BOI.js";import{t}from"./react-BPZ7Dl3u.js";import{t as n}from"./jsx-runtime-CndCZlyt.js";import{C as r,l as i}from"./index-wGOlH2Wi-BIV0ccqi.js";import{C as a}from"./context-DEETNFc9-CiNZw6c8.js";import{E as o,M as s}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{r as c}from"./eventemitter3-DqDdb4wS.js";import"./toViemAccount-Cy6jgXI2-BPF6Q9Ih.js";import{t as l}from"./modal-context-BAV3sQpp-k4X-kX2b.js";import{t as u}from"./createLucideIcon-BL8LZspr.js";import{t as d}from"./credit-card-lOleYWEf.js";import{c as f,i as p,l as m,v as h}from"./styles-DgsHCTKB-DTE6RwN7.js";import{r as g}from"./styles-DVyDvTdj-ipH1ixTV.js";var _=u(`banknote`,[[`rect`,{width:`20`,height:`12`,x:`2`,y:`6`,rx:`2`,key:`9lu3g6`}],[`circle`,{cx:`12`,cy:`12`,r:`2`,key:`1c9p78`}],[`path`,{d:`M6 12h.01M18 12h.01`,key:`113zkx`}]]),v=n(),y=e(t(),1);c(),s();var b={component:()=>{let e=r(),{onUserCloseViaDialogOrKeybindRef:t}=l(),n=a(),o=(0,y.useRef)(!1);(0,y.useEffect)((()=>{e&&(o.current=!1)}),[e]);let s=(0,y.useCallback)((async()=>{!o.current&&e&&(o.current=!0,i(),await e.onCancel())}),[e]);return(0,y.useEffect)((()=>(t.current=s,()=>{t.current===s&&(t.current=null)})),[s,t]),e?e.error?(0,v.jsx)(f,{icon:_,iconVariant:`warning`,title:`Unable to add funds`,subtitle:e.error,showClose:!0,onClose:s,primaryCta:{label:`Close`,onClick:s}}):(0,v.jsx)(f,{icon:_,iconVariant:`subtle`,title:`Select method`,subtitle:`Choose how to fund your wallet`,showClose:!0,onClose:s,children:(0,v.jsxs)(g,{style:{marginTop:`1rem`},$colorScheme:n.appearance.palette.colorScheme,children:[e.startFiat&&(0,v.jsxs)(p,{onClick:async()=>{o.current||(o.current=!0,await e.startFiat?.())},children:[(0,v.jsx)(x,{children:(0,v.jsx)(d,{})}),(0,v.jsxs)(S,{children:[(0,v.jsx)(m,{children:`Pay with fiat`}),(0,v.jsx)(C,{children:`Apple Pay, Google Pay, or debit card`})]})]}),e.startCrypto&&(0,v.jsxs)(p,{onClick:async()=>{o.current||(o.current=!0,await e.startCrypto?.())},children:[(0,v.jsx)(x,{children:(0,v.jsx)(h,{})}),(0,v.jsxs)(S,{children:[(0,v.jsx)(m,{children:`Transfer from wallet`}),(0,v.jsx)(C,{children:`Send crypto from any wallet`})]})]})]})}):null}},x=o.span`
  width: 2rem;
  height: 2rem;
  border-radius: var(--privy-border-radius-full);
  background-color: var(--privy-color-background-2);
  color: var(--color-icon-muted, #64668b);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;

  svg {
    width: 1.125rem;
    height: 1.125rem;
  }
`,S=o.span`
  display: flex;
  flex-direction: column;
  align-items: flex-start;
`,C=o.span`
  font-size: 0.875rem;
  line-height: 1.25rem;
  color: var(--privy-color-foreground-3);
`;export{b as AddFundsSelectionScreen,b as default};