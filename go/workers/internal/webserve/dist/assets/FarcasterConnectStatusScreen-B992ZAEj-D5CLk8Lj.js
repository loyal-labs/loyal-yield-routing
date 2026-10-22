import{s as e}from"./rolldown-runtime-Bjp52BOI.js";import{t}from"./react-BPZ7Dl3u.js";import{t as n}from"./jsx-runtime-CndCZlyt.js";import"./index-wGOlH2Wi-BIV0ccqi.js";import{C as r,v as i}from"./context-DEETNFc9-CiNZw6c8.js";import{E as a,Ht as o,M as s,Nt as c,b as l,pt as u,vt as d}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{r as f}from"./eventemitter3-DqDdb4wS.js";import"./toViemAccount-Cy6jgXI2-BPF6Q9Ih.js";import{t as p}from"./modal-context-BAV3sQpp-k4X-kX2b.js";import{t as m}from"./browser-CVjgFsl-.js";import{t as h}from"./check-CMoNoGI-.js";import{t as g}from"./copy-5KFdobpP.js";import{t as _}from"./ModalHeader-CAqKnddp-BVsHJbWY.js";import{t as v}from"./LabelXs-oqZNqbm_-LfXPARoT.js";import{t as y}from"./QrCode-BV7KfzAf-B8-EO96f.js";import{t as b}from"./ScreenLayout-Yoa2TSpi-GNprbNng.js";import{t as x}from"./shouldProceedtoEmbeddedWalletCreationFlow-BMHEqcS9-z4f-jrni.js";import{t as S}from"./farcaster-DPlSjvF5-CzBCmgyL.js";import{t as C}from"./OpenLink-DZHy38vr-DoJkUXTM.js";var w=n(),T=e(t(),1),E=e(s(),1);f(),m();var D=a.div`
  width: 100%;
`,O=a.div`
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.75rem;
  padding: 0.75rem;
  height: 56px;
  background: ${e=>e.$disabled?`var(--privy-color-background-2)`:`var(--privy-color-background)`};
  border: 1px solid var(--privy-color-foreground-4);
  border-radius: var(--privy-border-radius-md);

  &:hover {
    border-color: ${e=>e.$disabled?`var(--privy-color-foreground-4)`:`var(--privy-color-foreground-3)`};
  }
`,k=a.div`
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: center;
`,A=a.span`
  display: block;
  font-size: 16px;
  line-height: 24px;
  color: ${e=>e.$disabled?`var(--privy-color-foreground-2)`:`var(--privy-color-foreground)`};
  overflow: hidden;
  text-overflow: ellipsis;
  /* Use single-line truncation without nowrap to respect container width */
  display: -webkit-box;
  -webkit-line-clamp: 1;
  -webkit-box-orient: vertical;
  word-break: break-all;

  @media (min-width: 441px) {
    font-size: 14px;
    line-height: 20px;
  }
`,j=a(A)`
  color: var(--privy-color-foreground-3);
  font-style: italic;
`,M=a(v)`
  margin-bottom: 0.5rem;
`,N=a(_)`
  && {
    gap: 0.375rem;
    font-size: 14px;
    flex-shrink: 0;
  }
`,P=({value:e,title:t,placeholder:n,className:r,showCopyButton:i=!0,truncate:a,maxLength:o=40,disabled:s=!1})=>{let[c,l]=(0,T.useState)(!1),u=a&&e?((e,t,n)=>{if((e=e.startsWith(`https://`)?e.slice(8):e).length<=n)return e;if(t===`middle`){let t=Math.ceil(n/2)-2,r=Math.floor(n/2)-1;return`${e.slice(0,t)}...${e.slice(-r)}`}return`${e.slice(0,n-3)}...`})(e,a,o):e;return(0,T.useEffect)((()=>{if(c){let e=setTimeout((()=>l(!1)),3e3);return()=>clearTimeout(e)}}),[c]),(0,w.jsxs)(D,{className:r,children:[t&&(0,w.jsx)(M,{children:t}),(0,w.jsxs)(O,{$disabled:s,children:[(0,w.jsx)(k,{children:e?(0,w.jsx)(A,{$disabled:s,title:e,children:u}):(0,w.jsx)(j,{$disabled:s,children:n||`No value`})}),i&&e&&(0,w.jsx)(N,{onClick:function(t){t.stopPropagation(),navigator.clipboard.writeText(e).then((()=>l(!0))).catch(console.error)},size:`sm`,children:(0,w.jsxs)(w.Fragment,c?{children:[`Copied`,(0,w.jsx)(h,{size:14})]}:{children:[`Copy`,(0,w.jsx)(g,{size:14})]})})]})]})},F=({connectUri:e,loading:t,success:n,errorMessage:r,onBack:i,onClose:a,onOpenFarcaster:o})=>(0,w.jsx)(b,E.isMobile||t?E.isIOS?{title:r?r.message:`Sign in with Farcaster`,subtitle:r?r.detail:`To sign in with Farcaster, please open the Farcaster app.`,icon:S,iconVariant:`loading`,iconLoadingStatus:{success:n,fail:!!r},primaryCta:e&&o?{label:`Open Farcaster app`,onClick:o}:void 0,onBack:i,onClose:a,watermark:!0}:{title:r?r.message:`Signing in with Farcaster`,subtitle:r?r.detail:`This should only take a moment`,icon:S,iconVariant:`loading`,iconLoadingStatus:{success:n,fail:!!r},onBack:i,onClose:a,watermark:!0,children:e&&E.isMobile&&(0,w.jsx)(L,{children:(0,w.jsx)(C,{text:`Take me to Farcaster`,url:e,color:`#8a63d2`})})}:{title:`Sign in with Farcaster`,subtitle:`Scan with your phone's camera to continue.`,onBack:i,onClose:a,watermark:!0,children:(0,w.jsxs)(R,{children:[(0,w.jsx)(z,{children:e?(0,w.jsx)(y,{url:e,size:275,squareLogoElement:S}):(0,w.jsx)(H,{children:(0,w.jsx)(l,{})})}),(0,w.jsxs)(B,{children:[(0,w.jsx)(V,{children:`Or copy this link and paste it into a phone browser to open the Farcaster app.`}),e&&(0,w.jsx)(P,{value:e,truncate:`end`,maxLength:30,showCopyButton:!0,disabled:!0})]})]})}),I={component:()=>{let{authenticated:e,logout:t,ready:n,user:a}=c(),{lastScreen:s,navigate:l,navigateBack:f,setModalData:m}=p(),h=r(),{getAuthFlow:g,loginWithFarcaster:_,closePrivyModal:v,createAnalyticsEvent:y}=o(),[b,S]=(0,T.useState)(void 0),[C,E]=(0,T.useState)(!1),[D,O]=(0,T.useState)(!1),k=(0,T.useRef)([]),A=g(),j=A?.meta.connectUri;return(0,T.useEffect)((()=>{let e=Date.now(),t=setInterval((async()=>{let n=await A.pollForReady.execute(),r=Date.now()-e;if(n){clearInterval(t),E(!0);try{await _(),O(!0)}catch(e){let t={retryable:!1,message:`Authentication failed`};if(e?.privyErrorCode===d.ALLOWLIST_REJECTED)return void l(`AllowlistRejectionScreen`);if(e?.privyErrorCode===d.USER_LIMIT_REACHED)return console.error(new u(e).toString()),void l(`UserLimitReachedScreen`);if(e?.privyErrorCode===d.USER_DOES_NOT_EXIST)return void l(`AccountNotFoundScreen`);if(e?.privyErrorCode===d.LINKED_TO_ANOTHER_USER)t.detail=e.message??`This account has already been linked to another user.`;else{if(e?.privyErrorCode===d.ACCOUNT_TRANSFER_REQUIRED&&e.data?.data?.nonce)return m({accountTransfer:{nonce:e.data?.data?.nonce,account:e.data?.data?.subject,displayName:e.data?.data?.account?.displayName,linkMethod:`farcaster`,embeddedWalletAddress:e.data?.data?.otherUser?.embeddedWalletAddress,farcasterEmbeddedAddress:e.data?.data?.otherUser?.farcasterEmbeddedAddress}}),void l(`LinkConflictScreen`);e?.privyErrorCode===d.INVALID_CREDENTIALS?(t.retryable=!0,t.detail=`Something went wrong. Try again.`):e?.privyErrorCode===d.TOO_MANY_REQUESTS&&(t.detail=`Too many requests. Please wait before trying again.`)}S(t)}}else r>12e4&&(clearInterval(t),S({retryable:!0,message:`Authentication failed`,detail:`The request timed out. Try again.`}))}),2e3);return()=>{clearInterval(t),k.current.forEach((e=>clearTimeout(e)))}}),[]),(0,T.useEffect)((()=>{if(n&&e&&D&&a){if(h?.legal.requireUsersAcceptTerms&&!a.hasAcceptedTerms){let e=setTimeout((()=>{l(`AffirmativeConsentScreen`)}),i);return()=>clearTimeout(e)}D&&(x(a,h.embeddedWallets)?k.current.push(setTimeout((()=>{m({createWallet:{onSuccess:()=>{},onFailure:e=>{console.error(e),y({eventName:`embedded_wallet_creation_failure_logout`,payload:{error:e,screen:`FarcasterConnectStatusScreen`}}),t()},callAuthOnSuccessOnClose:!0}}),l(`EmbeddedWalletOnAccountCreateScreen`)}),1400)):k.current.push(setTimeout((()=>v({shouldCallAuthOnSuccess:!0,isSuccess:!0})),1400)))}}),[D,n,e,a]),(0,w.jsx)(F,{connectUri:j,loading:C,success:D,errorMessage:b,onBack:s?f:void 0,onClose:v,onOpenFarcaster:()=>{j&&(window.location.href=j)}})}},L=a.div`
  margin-top: 24px;
`,R=a.div`
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 24px;
`,z=a.div`
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 275px;
`,B=a.div`
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 16px;
`,V=a.div`
  font-size: 0.875rem;
  text-align: center;
  color: var(--privy-color-foreground-2);
`,H=a.div`
  position: relative;
  width: 82px;
  height: 82px;
`;export{I as FarcasterConnectStatusScreen,I as default,F as FarcasterConnectStatusView};