import{s as e}from"./rolldown-runtime-Bjp52BOI.js";import{t}from"./react-BPZ7Dl3u.js";import{t as n}from"./jsx-runtime-CndCZlyt.js";import{o as r}from"./index-wGOlH2Wi-BIV0ccqi.js";import"./context-DEETNFc9-CiNZw6c8.js";import{Dt as i,E as a,Ht as o,M as s,Nt as c,T as l,wt as u}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{r as d}from"./eventemitter3-DqDdb4wS.js";import"./toViemAccount-Cy6jgXI2-BPF6Q9Ih.js";import{t as f}from"./modal-context-BAV3sQpp-k4X-kX2b.js";import{l as p}from"./ModalHeader-CAqKnddp-BVsHJbWY.js";import{a as m,d as h,n as g,o as _,s as v}from"./shared-Cwq8m_h6-BKUljOfe.js";import{t as y}from"./ShieldCheckIcon-Dwz7-h54.js";import{t as b}from"./Screen-CLdp3cO6-TiE7z0fm.js";import{a as x}from"./Layouts-BlFm53ED-DPJJc7x7.js";var S=n(),C=e(t(),1);d(),s();var w={component:()=>{let[e,t]=(0,C.useState)(!0),{authenticated:n,user:a}=c(),{walletProxy:s,closePrivyModal:l,createAnalyticsEvent:d,client:p}=o(),{navigate:w,data:O,onUserCloseViaDialogOrKeybindRef:k}=f(),[A,j]=(0,C.useState)(void 0),[M,N]=(0,C.useState)(``),[P,F]=(0,C.useState)(!1),{entropyId:I,entropyIdVerifier:L,onCompleteNavigateTo:R,onSuccess:z,onFailure:B}=O.recoverWallet,V=(e=`User exited before their wallet could be recovered`)=>{l({shouldCallAuthOnSuccess:!1}),B(typeof e==`string`?new u(e):e)};return k.current=V,(0,C.useEffect)((()=>{if(!n)return V(`User must be authenticated and have a Privy wallet before it can be recovered`)}),[n]),(0,S.jsxs)(b,{children:[(0,S.jsx)(b.Header,{icon:y,title:`Enter your password`,subtitle:`Please provision your account on this new device. To continue, enter your recovery password.`,showClose:!0,onClose:V}),(0,S.jsx)(b.Body,{children:(0,S.jsx)(T,{children:(0,S.jsxs)(`div`,{children:[(0,S.jsxs)(m,{children:[(0,S.jsx)(_,{type:e?`password`:`text`,onChange:e=>(e=>{e&&j(e)})(e.target.value),disabled:P,style:{paddingRight:`2.3rem`}}),(0,S.jsx)(h,{style:{right:`0.75rem`},children:e?(0,S.jsx)(g,{onClick:()=>t(!1)}):(0,S.jsx)(v,{onClick:()=>t(!0)})})]}),!!M&&(0,S.jsx)(E,{children:M})]})})}),(0,S.jsxs)(b.Footer,{children:[(0,S.jsx)(b.HelpText,{children:(0,S.jsxs)(x,{children:[(0,S.jsx)(`h4`,{children:`Why is this necessary?`}),(0,S.jsx)(`p`,{children:`You previously set a password for this wallet. This helps ensure only you can access it`})]})}),(0,S.jsx)(b.Actions,{children:(0,S.jsx)(D,{loading:P||!s,disabled:!A,onClick:async()=>{F(!0);let e=await p.getAccessToken(),t=i(a,I);if(!e||!t||A===null)return V(`User must be authenticated and have a Privy wallet before it can be recovered`);try{d({eventName:`embedded_wallet_recovery_started`,payload:{walletAddress:t.address}}),await s?.recover({accessToken:e,entropyId:I,entropyIdVerifier:L,recoveryPassword:A}),N(``),R?w(R):l({shouldCallAuthOnSuccess:!1}),z?.(t),d({eventName:`embedded_wallet_recovery_completed`,payload:{walletAddress:t.address}})}catch(e){r(e)?N(`Invalid recovery password, please try again.`):N(`An error has occurred, please try again.`)}finally{F(!1)}},$hideAnimations:!I&&P,children:`Recover your account`})}),(0,S.jsx)(b.Watermark,{})]})]})}},T=a.div`
  display: flex;
  flex-direction: column;
  gap: 1.5rem;
`,E=a.div`
  line-height: 20px;
  height: 20px;
  font-size: 13px;
  color: var(--privy-color-error);
  text-align: left;
  margin-top: 0.5rem;
`,D=a(p)`
  ${({$hideAnimations:e})=>e&&l`
      && {
        // Remove animations because the recoverWallet task on the iframe partially
        // blocks the renderer, so the animation stutters and doesn't look good
        transition: none;
      }
    `}
`;export{w as PasswordRecoveryScreen,w as default};