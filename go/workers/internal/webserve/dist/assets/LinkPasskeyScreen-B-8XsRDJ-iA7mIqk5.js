import{r as e,s as t}from"./rolldown-runtime-Bjp52BOI.js";import{t as n}from"./react-BPZ7Dl3u.js";import{t as r}from"./jsx-runtime-CndCZlyt.js";import"./context-DEETNFc9-CiNZw6c8.js";import{Ct as i,E as a,Ht as o,M as s,Nt as c,T as l,g as u,vt as d}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{s as f}from"./usePrivy-WvWzevM9-DgTN2pd0.js";import{r as p}from"./eventemitter3-DqDdb4wS.js";import{t as m}from"./modal-context-BAV3sQpp-k4X-kX2b.js";import{t as h}from"./createLucideIcon-BL8LZspr.js";import{t as g}from"./circle-check-big-_-d3dkXN.js";import{t as _}from"./fingerprint-pattern-B9xrFhwF.js";import{t as v}from"./ScreenLayout-Yoa2TSpi-GNprbNng.js";import{n as y,t as b}from"./TodoList-CgrU7uwu-DsOq-_wh.js";var x=h(`trash-2`,[[`path`,{d:`M10 11v6`,key:`nco0om`}],[`path`,{d:`M14 11v6`,key:`outv1u`}],[`path`,{d:`M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6`,key:`miytrc`}],[`path`,{d:`M3 6h18`,key:`d0wm0j`}],[`path`,{d:`M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2`,key:`e791ji`}]]),S=e({DoubleIconWrapper:()=>N,LinkButton:()=>F,LinkPasskeyScreen:()=>M,LinkPasskeyView:()=>T,default:()=>M}),C=r(),w=t(n(),1);p(),s();var T=({passkeys:e,name:t,isLoading:n,errorReason:r,success:i,expanded:a,onLinkPasskey:o,onUnlinkPasskey:s,onExpand:c,onBack:l,onClose:u})=>i?(0,C.jsx)(v,{title:`Passkeys updated`,icon:g,iconVariant:`success`,primaryCta:{label:`Done`,onClick:u},onClose:u,watermark:!0}):a?(0,C.jsx)(v,{icon:_,title:`Your passkeys`,onBack:l,onClose:u,watermark:!0,children:(0,C.jsx)(A,{passkeys:e,expanded:a,onUnlink:s,onExpand:c})}):(0,C.jsxs)(v,{icon:_,title:`Set up passkey verification`,subtitle:`Verify with passkey`,primaryCta:{label:`Add new passkey`,onClick:o,loading:n},onClose:u,watermark:!0,helpText:r||void 0,children:[e.length===0?(0,C.jsx)(j,{}):(0,C.jsx)(E,{children:(0,C.jsx)(A,{passkeys:e,expanded:a,onUnlink:s,onExpand:c})}),t?(0,C.jsxs)(D,{children:[(0,C.jsx)(O,{children:`New Passkey Name`}),(0,C.jsx)(k,{children:t})]}):null]}),E=a.div`
  margin-bottom: 0.75rem;
`,D=a.div`
  margin-top: 0.25rem;
`,O=a.div`
  color: var(--privy-color-foreground-2);
  font-size: 0.75rem;
  font-weight: 500;
  line-height: 1rem;
  margin-bottom: 0.25rem;
`,k=a.div`
  color: var(--privy-color-foreground);
  font-size: 0.875rem;
  line-height: 1.25rem;
`,A=({passkeys:e,expanded:t,onUnlink:n,onExpand:r})=>{let[i,a]=(0,w.useState)([]),o=t?e.length:2;return(0,C.jsxs)(`div`,{children:[(0,C.jsx)(L,{children:`Your passkeys`}),(0,C.jsxs)(I,{children:[e.slice(0,o).map((e=>{return(0,C.jsxs)(B,{children:[(0,C.jsxs)(`div`,{children:[(0,C.jsx)(R,{children:(t=e,t.authenticatorName?t.createdWithBrowser?`${t.authenticatorName} on ${t.createdWithBrowser}`:t.authenticatorName:t.createdWithBrowser?t.createdWithOs?`${t.createdWithBrowser} on ${t.createdWithOs}`:`${t.createdWithBrowser}`:`Unknown device`)}),(0,C.jsxs)(z,{children:[`Last used:`,` `,(e.latestVerifiedAt??e.firstVerifiedAt)?.toLocaleString()??`N/A`]})]}),(0,C.jsx)(H,{disabled:i.includes(e.credentialId),onClick:()=>(async e=>{a((t=>t.concat([e]))),await n(e),a((t=>t.filter((t=>t!==e))))})(e.credentialId),children:i.includes(e.credentialId)?(0,C.jsx)(u,{}):(0,C.jsx)(x,{size:16})})]},e.credentialId);var t})),e.length>2&&!t&&(0,C.jsx)(F,{onClick:r,children:`View all`})]})]})},j=()=>(0,C.jsxs)(b,{style:{color:`var(--privy-color-foreground)`},children:[(0,C.jsx)(y,{children:`Verify with Touch ID, Face ID, PIN, or hardware key`}),(0,C.jsx)(y,{children:`Takes seconds to set up and use`}),(0,C.jsx)(y,{children:`Use your passkey to verify transactions and login to your account`})]}),M={component:()=>{let{user:e}=c(),{unlink:t}=f(),{linkWithPasskey:n,closePrivyModal:r}=o(),{data:a}=m(),s=e?.linkedAccounts.filter((e=>e.type===`passkey`)),[l,u]=(0,w.useState)(!1),[p,h]=(0,w.useState)(``),[g,_]=(0,w.useState)(!1),[v,y]=(0,w.useState)(!1);return(0,w.useEffect)((()=>{s.length===0&&y(!1)}),[s.length]),(0,C.jsx)(T,{passkeys:s,name:a?.passkeyAuthModalData?.name,isLoading:l,errorReason:p,success:g,expanded:v,onLinkPasskey:()=>{u(!0),n({name:a?.passkeyAuthModalData?.name}).then((()=>_(!0))).catch((e=>{if(e instanceof i){if(e.privyErrorCode===d.CANNOT_LINK_MORE_OF_TYPE)return void h(`Cannot link more passkeys to account.`);if(e.privyErrorCode===d.PASSKEY_NOT_ALLOWED)return void h(`Passkey request timed out or rejected by user.`)}h(`Unknown error occurred.`)})).finally((()=>{u(!1)}))},onUnlinkPasskey:async e=>(u(!0),await t({credentialId:e}).then((()=>_(!0))).catch((e=>{e instanceof i&&e.privyErrorCode===d.MISSING_MFA_CREDENTIALS?h(`Cannot unlink a passkey enrolled in MFA`):h(`Unknown error occurred.`)})).finally((()=>{u(!1)}))),onExpand:()=>y(!0),onBack:()=>y(!1),onClose:()=>r()})}},N=a.div`
  display: flex;
  align-items: center;
  justify-content: center;
  width: 180px;
  height: 90px;
  border-radius: 50%;
  svg + svg {
    margin-left: 12px;
  }
  > svg {
    z-index: 2;
    color: var(--privy-color-accent) !important;
    stroke: var(--privy-color-accent) !important;
    fill: var(--privy-color-accent) !important;
  }
`,P=l`
  && {
    width: 100%;
    font-size: 0.875rem;
    line-height: 1rem;

    /* Tablet and Up */
    @media (min-width: 440px) {
      font-size: 14px;
    }

    display: flex;
    gap: 12px;
    justify-content: center;

    padding: 6px 8px;
    background-color: var(--privy-color-background);
    transition: background-color 200ms ease;
    color: var(--privy-color-accent) !important;

    :focus {
      outline: none;
      box-shadow: none;
    }
  }
`,F=a.button`
  ${P}
`,I=a.div`
  display: flex;
  flex-direction: column;
  align-items: stretch;
  gap: 0.8rem;
  padding: 0.5rem 0rem 0rem;
  flex-grow: 1;
  width: 100%;
`,L=a.div`
  line-height: 20px;
  height: 20px;
  font-size: 1em;
  font-weight: 450;
  display: flex;
  justify-content: flex-beginning;
  width: 100%;
`,R=a.div`
  font-size: 1em;
  line-height: 1.3em;
  font-weight: 500;
  color: var(--privy-color-foreground-2);
  padding: 0.2em 0;
`,z=a.div`
  font-size: 0.875rem;
  line-height: 1rem;
  color: #64668b;
  padding: 0.2em 0;
`,B=a.div`
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 1em;
  gap: 10px;
  font-size: 0.875rem;
  line-height: 1rem;
  text-align: left;
  border-radius: 8px;
  border: 1px solid #e2e3f0 !important;
  width: 100%;
  height: 5em;
`,V=l`
  :focus,
  :hover,
  :active {
    outline: none;
  }
  display: flex;
  width: 2em;
  height: 2em;
  justify-content: center;
  align-items: center;
  svg {
    color: var(--privy-color-error);
  }
  svg:hover {
    color: var(--privy-color-foreground-3);
  }
`,H=a.button`
  ${V}
`;export{F as n,S as r,N as t};