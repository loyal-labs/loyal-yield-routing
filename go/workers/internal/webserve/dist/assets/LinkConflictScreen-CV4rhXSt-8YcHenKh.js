import{s as e}from"./rolldown-runtime-Bjp52BOI.js";import{t}from"./react-BPZ7Dl3u.js";import{t as n}from"./jsx-runtime-CndCZlyt.js";import{C as r}from"./context-DEETNFc9-CiNZw6c8.js";import{E as i,Ht as a,M as o}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{r as s}from"./eventemitter3-DqDdb4wS.js";import{t as c}from"./modal-context-BAV3sQpp-k4X-kX2b.js";import{l,r as u,t as d,u as f}from"./ModalHeader-CAqKnddp-BVsHJbWY.js";import{t as p}from"./Address-BNlptHuN-Bz4rV4Aa.js";import{t as m}from"./ExclamationCircleIcon-CBwANIin.js";import{t as h}from"./ExclamationTriangleIcon-ClPI_H_M.js";import{t as g}from"./WalletIcon-BR0M0yyy.js";import{t as _}from"./StackedContainer-B2vaEl56-CDSbBEdr.js";import{t as v}from"./capitalizeFirstLetter-DmLYqXsO-DtOXx-2n.js";var y=n(),b=e(t());function x({title:e,titleId:t,...n},r){return b.createElement(`svg`,Object.assign({xmlns:`http://www.w3.org/2000/svg`,fill:`none`,viewBox:`0 0 24 24`,strokeWidth:1.5,stroke:`currentColor`,"aria-hidden":`true`,"data-slot":`icon`,ref:r,"aria-labelledby":t},n),e?b.createElement(`title`,{id:t},e):null,b.createElement(`path`,{strokeLinecap:`round`,strokeLinejoin:`round`,d:`M16.5 8.25V6a2.25 2.25 0 0 0-2.25-2.25H6A2.25 2.25 0 0 0 3.75 6v8.25A2.25 2.25 0 0 0 6 16.5h2.25m8.25-8.25H18a2.25 2.25 0 0 1 2.25 2.25V18A2.25 2.25 0 0 1 18 20.25h-7.5A2.25 2.25 0 0 1 8.25 18v-1.5m8.25-8.25h-6a2.25 2.25 0 0 0-2.25 2.25v6`}))}var S=b.forwardRef(x);s(),o();var C=i.span`
  && {
    width: 82px;
    height: 82px;
    border-width: 4px;
    border-style: solid;
    border-color: ${e=>e.color??`var(--privy-color-accent)`};
    border-bottom-color: transparent;
    border-radius: 50%;
    display: inline-block;
    box-sizing: border-box;
    animation: rotation 1.2s linear infinite;
    transition: border-color 800ms;
    border-bottom-color: ${e=>e.color??`var(--privy-color-accent)`};
  }
`;function w(e){return(0,y.jsxs)(`svg`,{xmlns:`http://www.w3.org/2000/svg`,width:`24`,height:`24`,viewBox:`0 0 24 24`,fill:`none`,stroke:`currentColor`,"stroke-width":`2`,"stroke-linecap":`round`,"stroke-linejoin":`round`,...e,children:[(0,y.jsx)(`circle`,{cx:`12`,cy:`12`,r:`10`}),(0,y.jsx)(`line`,{x1:`12`,x2:`12`,y1:`8`,y2:`12`}),(0,y.jsx)(`line`,{x1:`12`,x2:`12.01`,y1:`16`,y2:`16`})]})}var T=({onTransfer:e,isTransferring:t,transferSuccess:n})=>(0,y.jsx)(l,{...n?{success:!0,children:`Success!`}:{warn:!0,loading:t,onClick:e,children:`Transfer and delete account`}}),E=i.div`
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  width: 100%;
  padding-bottom: 16px;
`,D=i.div`
  display: flex;
  flex-direction: column;
  && p {
    font-size: 14px;
  }
  width: 100%;
  gap: 16px;
`,O=i.div`
  display: flex;
  cursor: pointer;
  align-items: center;
  width: 100%;
  border: 1px solid var(--privy-color-foreground-4) !important;
  border-radius: var(--privy-border-radius-md);
  padding: 8px 10px;
  font-size: 14px;
  font-weight: 500;
  gap: 8px;
`,k=i(m)`
  position: relative;
  width: ${({$iconSize:e})=>`${e}px`};
  height: ${({$iconSize:e})=>`${e}px`};
  color: var(--privy-color-foreground-3);
  margin-left: auto;
`,A=i(S)`
  position: relative;
  width: 15px;
  height: 15px;
  color: var(--privy-color-foreground-3);
  margin-left: auto;
`,j=i.ol`
  display: flex;
  flex-direction: column;
  font-size: 14px;
  width: 100%;
  text-align: left;
`,M=i.li`
  font-size: 14px;
  list-style-type: auto;
  list-style-position: outside;
  margin-left: 1rem;
  margin-bottom: 0.5rem; /* Adjust the margin as needed */

  &:last-child {
    margin-bottom: 0; /* Remove margin from the last item */
  }
`,N=i.div`
  position: relative;
  width: 60px;
  height: 60px;
  margin: 10px;
  display: flex;
  justify-content: center;
  align-items: center;
`,P=()=>(0,y.jsx)(N,{children:(0,y.jsx)(k,{$iconSize:60})}),F=({address:e,onClose:t,onRetry:n,onTransfer:i,isTransferring:a,transferSuccess:o})=>{let{defaultChain:s}=r(),c=s.blockExplorers?.default.url??`https://etherscan.io`;return(0,y.jsxs)(y.Fragment,{children:[(0,y.jsx)(u,{onClose:t,backFn:n}),(0,y.jsxs)(E,{children:[(0,y.jsx)(P,{}),(0,y.jsxs)(D,{children:[(0,y.jsx)(`h3`,{children:`Check account assets before transferring`}),(0,y.jsx)(`p`,{children:`Before transferring, ensure there are no assets in the other account. Assets in that account will not transfer automatically and may be lost.`}),(0,y.jsxs)(j,{children:[(0,y.jsx)(`p`,{children:` To check your balance, you can:`}),(0,y.jsx)(M,{children:`Log out and log back into the other account, or `}),(0,y.jsxs)(M,{children:[`Copy your wallet address and use a`,` `,(0,y.jsx)(`u`,{children:(0,y.jsx)(`a`,{target:`_blank`,href:c,children:`block explorer`})}),` `,`to see if the account holds any assets.`]})]}),(0,y.jsxs)(O,{onClick:()=>navigator.clipboard.writeText(e).catch(console.error),children:[(0,y.jsx)(g,{color:`var(--privy-color-foreground-1)`,strokeWidth:2,height:`28px`,width:`28px`}),(0,y.jsx)(p,{address:e,showCopyIcon:!1}),(0,y.jsx)(A,{})]}),(0,y.jsx)(T,{onTransfer:i,isTransferring:a,transferSuccess:o})]})]}),(0,y.jsx)(f,{})]})},I={component:()=>{let{initiateAccountTransfer:e,closePrivyModal:t}=a(),{data:n,navigate:r,lastScreen:i,setModalData:o}=c(),[s,l]=(0,b.useState)(void 0),[u,d]=(0,b.useState)(!1),[f,p]=(0,b.useState)(!1),m=async()=>{try{if(!n?.accountTransfer?.nonce||!n?.accountTransfer?.account)throw Error(`missing account transfer inputs`);p(!0),await e({nonce:n?.accountTransfer?.nonce,account:n?.accountTransfer?.account,accountType:n?.accountTransfer?.linkMethod,externalWalletMetadata:n?.accountTransfer?.externalWalletMetadata,telegramWebAppData:n?.accountTransfer?.telegramWebAppData,telegramAuthResult:n?.accountTransfer?.telegramAuthResult,farcasterEmbeddedAddress:n?.accountTransfer?.farcasterEmbeddedAddress,oAuthUserInfo:n?.accountTransfer?.oAuthUserInfo}),d(!0),p(!1),setTimeout(t,1e3)}catch(e){o({errorModalData:{error:e,previousScreen:i||`LinkConflictScreen`}}),r(`ErrorScreen`,!0)}};return s?(0,y.jsx)(F,{address:s,onClose:t,onRetry:()=>l(void 0),onTransfer:m,isTransferring:f,transferSuccess:u}):(0,y.jsx)(L,{onClose:t,onInfo:()=>l(n?.accountTransfer?.embeddedWalletAddress),onContinue:()=>l(n?.accountTransfer?.embeddedWalletAddress),onTransfer:m,isTransferring:f,transferSuccess:u,data:n})}},L=({onClose:e,onContinue:t,onInfo:n,onTransfer:r,transferSuccess:i,isTransferring:a,data:o})=>{if(!o?.accountTransfer?.linkMethod||!o?.accountTransfer?.displayName)return;let s={method:o?.accountTransfer?.linkMethod,handle:o?.accountTransfer?.displayName,disclosedAccount:o?.accountTransfer?.embeddedWalletAddress?{type:`wallet`,handle:o?.accountTransfer?.embeddedWalletAddress}:void 0};return(0,y.jsxs)(y.Fragment,{children:[(0,y.jsx)(u,{closeable:!0}),(0,y.jsxs)(E,{children:[(0,y.jsx)(_,{children:(0,y.jsxs)(`div`,{children:[(0,y.jsx)(C,{color:`var(--privy-color-error)`}),(0,y.jsx)(h,{height:38,width:38,stroke:`var(--privy-color-error)`})]})}),(0,y.jsxs)(D,{children:[(0,y.jsxs)(`h3`,{children:[function(e){switch(e){case`sms`:return`Phone number`;case`email`:return`Email address`;case`siwe`:return`Wallet address`;case`siws`:return`Solana wallet address`;case`linkedin`:return`LinkedIn profile`;case`google`:case`apple`:case`discord`:case`github`:case`instagram`:case`spotify`:case`tiktok`:case`line`:case`twitch`:case`twitter`:case`telegram`:case`farcaster`:return`${v(e.replace(`_oauth`,``))} profile`;default:return e.startsWith(`privy:`)?`Cross-app account`:e}}(s.method),` is associated with another account`]}),(0,y.jsxs)(`p`,{children:[`Do you want to transfer`,(0,y.jsx)(`b`,{children:s.handle?` ${s.handle}`:``}),` to this account instead? This will delete your other account.`]}),(0,y.jsx)(R,{onClick:n,disclosedAccount:s.disclosedAccount})]}),(0,y.jsxs)(D,{style:{gap:12,marginTop:12},children:[o?.accountTransfer?.embeddedWalletAddress?(0,y.jsx)(l,{onClick:t,children:`Continue`}):(0,y.jsx)(T,{onTransfer:r,transferSuccess:i,isTransferring:a}),(0,y.jsx)(d,{onClick:e,children:`No thanks`})]})]}),(0,y.jsx)(f,{})]})};function R({disclosedAccount:e,onClick:t}){return e?(0,y.jsxs)(O,{onClick:t,children:[(0,y.jsx)(g,{color:`var(--privy-color-foreground-1)`,strokeWidth:2,height:`28px`,width:`28px`}),(0,y.jsx)(p,{address:e.handle,showCopyIcon:!1}),(0,y.jsx)(w,{width:15,height:15,color:`var(--privy-color-foreground-3)`,style:{marginLeft:`auto`}})]}):null}export{I as LinkConflictScreen,I as default,L as LinkConflictScreenView};