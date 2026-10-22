import{s as e}from"./rolldown-runtime-Bjp52BOI.js";import{t}from"./react-BPZ7Dl3u.js";import{t as n}from"./jsx-runtime-CndCZlyt.js";import{C as r,v as i}from"./context-DEETNFc9-CiNZw6c8.js";import{E as a,Ht as o,M as s,b as c}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{r as l}from"./eventemitter3-DqDdb4wS.js";import{t as u}from"./modal-context-BAV3sQpp-k4X-kX2b.js";import{t as d}from"./browser-CVjgFsl-.js";import{t as f}from"./QrCode-BV7KfzAf-B8-EO96f.js";import{t as p}from"./ScreenLayout-Yoa2TSpi-GNprbNng.js";import{t as m}from"./farcaster-DPlSjvF5-CzBCmgyL.js";import{t as h}from"./OpenLink-DZHy38vr-DoJkUXTM.js";import{t as g}from"./CopyToClipboard-DSTf_eKU-DbDRMmls.js";var _=n(),v=e(t(),1),y=e(s(),1);l(),d();var b=`#8a63d2`,x=({appName:e,loading:t,success:n,errorMessage:r,connectUri:i,onBack:a,onClose:o,onOpenFarcaster:s})=>(0,_.jsx)(p,y.isMobile||t?y.isIOS?{title:r?r.message:`Add a signer to Farcaster`,subtitle:r?r.detail:`This will allow ${e} to add casts, likes, follows, and more on your behalf.`,icon:m,iconVariant:`loading`,iconLoadingStatus:{success:n,fail:!!r},primaryCta:i&&s?{label:`Open Farcaster app`,onClick:s}:void 0,onBack:a,onClose:o,watermark:!0}:{title:r?r.message:`Requesting signer from Farcaster`,subtitle:r?r.detail:`This should only take a moment`,icon:m,iconVariant:`loading`,iconLoadingStatus:{success:n,fail:!!r},onBack:a,onClose:o,watermark:!0,children:i&&y.isMobile&&(0,_.jsx)(S,{children:(0,_.jsx)(h,{text:`Take me to Farcaster`,url:i,color:b})})}:{title:`Add a signer to Farcaster`,subtitle:`This will allow ${e} to add casts, likes, follows, and more on your behalf.`,onBack:a,onClose:o,watermark:!0,children:(0,_.jsxs)(C,{children:[(0,_.jsx)(w,{children:i?(0,_.jsx)(f,{url:i,size:275,squareLogoElement:m}):(0,_.jsx)(D,{children:(0,_.jsx)(c,{})})}),(0,_.jsxs)(T,{children:[(0,_.jsx)(E,{children:`Or copy this link and paste it into a phone browser to open the Farcaster app.`}),i&&(0,_.jsx)(g,{text:i,itemName:`link`,color:b})]})]})}),S=a.div`
  margin-top: 24px;
`,C=a.div`
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 24px;
`,w=a.div`
  padding: 24px;
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 275px;
`,T=a.div`
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 16px;
`,E=a.div`
  font-size: 0.875rem;
  text-align: center;
  color: var(--privy-color-foreground-2);
`,D=a.div`
  position: relative;
  width: 82px;
  height: 82px;
`,O={component:()=>{let{lastScreen:e,navigateBack:t,data:n}=u(),a=r(),{requestFarcasterSignerStatus:s,closePrivyModal:c}=o(),[l,d]=(0,v.useState)(void 0),[f,p]=(0,v.useState)(!1),[m,h]=(0,v.useState)(!1),g=(0,v.useRef)([]),y=n?.farcasterSigner;(0,v.useEffect)((()=>{let e=Date.now(),t=setInterval((async()=>{if(!y?.public_key)return clearInterval(t),void d({retryable:!0,message:`Connect failed`,detail:`Something went wrong. Please try again.`});y.status===`approved`&&(clearInterval(t),p(!1),h(!0),g.current.push(setTimeout((()=>c({shouldCallAuthOnSuccess:!1,isSuccess:!0})),1400)));let n=await s(y?.public_key),r=Date.now()-e;n.status===`approved`?(clearInterval(t),p(!1),h(!0),g.current.push(setTimeout((()=>c({shouldCallAuthOnSuccess:!1,isSuccess:!0})),i))):r>3e5?(clearInterval(t),d({retryable:!0,message:`Connect failed`,detail:`The request timed out. Try again.`})):n.status===`revoked`&&(clearInterval(t),d({retryable:!0,message:`Request rejected`,detail:`The request was rejected. Please try again.`}))}),2e3);return()=>{clearInterval(t),g.current.forEach((e=>clearTimeout(e)))}}),[]);let b=y?.status===`pending_approval`?y.signer_approval_url:void 0;return(0,_.jsx)(x,{appName:a.name,loading:f,success:m,errorMessage:l,connectUri:b,onBack:e?t:void 0,onClose:c,onOpenFarcaster:()=>{b&&(window.location.href=b)}})}};export{O as FarcasterSignerStatusScreen,O as default,x as FarcasterSignerStatusView};