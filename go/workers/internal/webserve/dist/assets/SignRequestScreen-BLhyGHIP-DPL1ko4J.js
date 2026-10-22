import{s as e}from"./rolldown-runtime-Bjp52BOI.js";import{t}from"./react-BPZ7Dl3u.js";import{t as n}from"./jsx-runtime-CndCZlyt.js";import{l as r,v as i}from"./context-DEETNFc9-CiNZw6c8.js";import{$t as a,E as o,G as s,H as c,Ht as l,M as u,Nt as d}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{Un as f,or as p}from"./ccip-CDRmV2C3.js";import{r as m}from"./eventemitter3-DqDdb4wS.js";import{r as h}from"./esm-DJNUAQDg.js";import{t as g}from"./modal-context-BAV3sQpp-k4X-kX2b.js";import{t as _}from"./createLucideIcon-BL8LZspr.js";import{t as v}from"./ScreenLayout-Yoa2TSpi-GNprbNng.js";import{t as y}from"./Layouts-BlFm53ED-DPJJc7x7.js";import{t as b}from"./CopyToClipboard-DSTf_eKU-DbDRMmls.js";import{n as x,t as S}from"./JsonTree-aPaJmPx7-Bm_iN2Nj.js";var C=_(`square-pen`,[[`path`,{d:`M12 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7`,key:`1m0v6g`}],[`path`,{d:`M18.375 2.625a1 1 0 0 1 3 3l-9.013 9.014a2 2 0 0 1-.853.505l-2.873.84a.5.5 0 0 1-.62-.62l.84-2.873a2 2 0 0 1 .506-.852z`,key:`ohrbg2`}]]),w=n(),T=e(t(),1);m(),u();var E=o.img`
  && {
    height: ${e=>e.size===`sm`?`65px`:`140px`};
    width: ${e=>e.size===`sm`?`65px`:`140px`};
    border-radius: 16px;
    margin-bottom: 12px;
  }
`,D=e=>{if(!p(e))return e;try{let t=f(e);return t.includes(`�`)?e:t}catch{return e}},O=e=>{try{let t=h.decode(e),n=new TextDecoder().decode(t);return n.includes(`�`)?e:n}catch{return e}},k=e=>{let{types:t,primaryType:n,...r}=e.typedData;return(0,w.jsxs)(w.Fragment,{children:[(0,w.jsx)(P,{data:r}),(0,w.jsx)(b,{text:(i=e.typedData,JSON.stringify(i,null,2)),itemName:`full payload to clipboard`}),` `]});var i},A=({method:e,messageData:t,copy:n,iconUrl:r,isLoading:i,success:a,walletProxyIsLoading:o,errorMessage:s,isCancellable:c,onSign:l,onCancel:u,onClose:d})=>(0,w.jsx)(v,{title:n.title,subtitle:n.description,showClose:!0,onClose:d,icon:C,iconVariant:`subtle`,helpText:s?(0,w.jsx)(N,{children:s}):void 0,primaryCta:{label:n.buttonText,onClick:l,disabled:i||a||o,loading:i},secondaryCta:c?{label:`Not now`,onClick:u,disabled:i||a||o}:void 0,watermark:!0,children:(0,w.jsxs)(y,{children:[r?(0,w.jsx)(E,{style:{alignSelf:`center`},size:`sm`,src:r,alt:`app image`}):null,(0,w.jsxs)(M,{children:[e===`personal_sign`&&(0,w.jsx)(F,{children:D(t)}),e===`eth_signTypedData_v4`&&(0,w.jsx)(k,{typedData:t}),e===`solana_signMessage`&&(0,w.jsx)(F,{children:O(t)})]})]})}),j={component:()=>{let{authenticated:e}=d(),{initializeWalletProxy:t,closePrivyModal:n}=l(),{navigate:o,data:u,onUserCloseViaDialogOrKeybindRef:f}=g(),[p,m]=(0,T.useState)(!0),[h,_]=(0,T.useState)(``),[v,y]=(0,T.useState)(),[b,x]=(0,T.useState)(null),[S,C]=(0,T.useState)(!1);(0,T.useEffect)((()=>{e||o(`LandingScreen`)}),[e]),(0,T.useEffect)((()=>{t(r).then((e=>{m(!1),e||(_(`An error has occurred, please try again.`),y(new s(new c(h,a.E32603_DEFAULT_INTERNAL_ERROR.eipCode))))}))}),[]);let{method:E,data:D,confirmAndSign:O,onSuccess:k,onFailure:j,uiOptions:M}=u.signMessage,N={title:M?.title||`Sign message`,description:M?.description||`Signing this message will not cost you any fees.`,buttonText:M?.buttonText||`Sign and continue`},P=e=>{e?k(e):j(v||new s(new c(`The user rejected the request.`,a.E4001_USER_REJECTED_REQUEST.eipCode))),n({shouldCallAuthOnSuccess:!1}),setTimeout((()=>{x(null),_(``),y(void 0)}),200)};return f.current=()=>{P(b)},(0,w.jsx)(A,{method:E,messageData:D,copy:N,iconUrl:M?.iconUrl&&typeof M.iconUrl==`string`?M.iconUrl:void 0,isLoading:S,success:b!==null,walletProxyIsLoading:p,errorMessage:h,isCancellable:M?.isCancellable,onSign:async()=>{C(!0),_(``);try{let e=await O();x(e),C(!1),setTimeout((()=>{P(e)}),i)}catch(e){console.error(e),_(`An error has occurred, please try again.`),y(new s(new c(h,a.E32603_DEFAULT_INTERNAL_ERROR.eipCode))),C(!1)}},onCancel:()=>P(null),onClose:()=>P(b)})}},M=o.div`
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 16px;
`,N=o.p`
  && {
    margin: 0;
    width: 100%;
    text-align: center;
    color: var(--privy-color-error-dark);
    font-size: 14px;
    line-height: 22px;
  }
`,P=o(S)`
  margin-top: 0;
`,F=o(x)`
  margin-top: 0;
`;export{j as SignRequestScreen,j as default,A as SignRequestView};