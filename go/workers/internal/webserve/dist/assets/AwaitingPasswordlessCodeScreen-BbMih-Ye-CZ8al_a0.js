import{s as e}from"./rolldown-runtime-Bjp52BOI.js";import{t}from"./react-BPZ7Dl3u.js";import{t as n}from"./jsx-runtime-CndCZlyt.js";import"./index-wGOlH2Wi-BIV0ccqi.js";import{C as r,v as i}from"./context-DEETNFc9-CiNZw6c8.js";import{E as a,Ht as o,M as s,Nt as c,pt as l,vt as u,xt as d}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{r as f}from"./eventemitter3-DqDdb4wS.js";import"./toViemAccount-Cy6jgXI2-BPF6Q9Ih.js";import{t as p}from"./modal-context-BAV3sQpp-k4X-kX2b.js";import{t as m}from"./EnvelopeIcon-Cb4FZ6mS.js";import{t as h}from"./PhoneIcon-Ch-j7Mre.js";import{t as g}from"./Link-DJ5gq9Di-0oyFLqpd.js";import{t as _}from"./ScreenLayout-Yoa2TSpi-GNprbNng.js";import{t as v}from"./shouldProceedtoEmbeddedWalletCreationFlow-BMHEqcS9-z4f-jrni.js";import{s as y}from"./Layouts-BlFm53ED-DPJJc7x7.js";var b=n(),x=e(t());function S({title:e,titleId:t,...n},r){return x.createElement(`svg`,Object.assign({xmlns:`http://www.w3.org/2000/svg`,viewBox:`0 0 20 20`,fill:`currentColor`,"aria-hidden":`true`,"data-slot":`icon`,ref:r,"aria-labelledby":t},n),e?x.createElement(`title`,{id:t},e):null,x.createElement(`path`,{fillRule:`evenodd`,d:`M16.704 4.153a.75.75 0 0 1 .143 1.052l-8 10.5a.75.75 0 0 1-1.127.075l-4.5-4.5a.75.75 0 0 1 1.06-1.06l3.894 3.893 7.48-9.817a.75.75 0 0 1 1.05-.143Z`,clipRule:`evenodd`}))}var C=x.forwardRef(S),w=e(s(),1);f();var T=({contactMethod:e,authFlow:t,emailDomain:n,appName:r=`Privy`,whatsAppEnabled:i=!1,onBack:a,onCodeSubmit:o,onResend:s,errorMessage:c,success:l=!1,resendCountdown:u=0,onInvalidInput:d,onClearError:f})=>{let[p,v]=(0,x.useState)(D);(0,x.useEffect)((()=>{c||v(D)}),[c]);let S=async e=>{e.preventDefault();let t=e.currentTarget.value.replace(` `,``);if(t===``)return;if(isNaN(Number(t)))return void d?.(`Code should be numeric`);f?.();let n=Number(e.currentTarget.name?.charAt(5)),r=[...t||[``]].slice(0,E-n),i=[...p.slice(0,n),...r,...p.slice(n+r.length)];v(i);let a=Math.min(Math.max(n+r.length,0),E-1);isNaN(Number(e.currentTarget.value))||document.querySelector(`input[name=code-${a}]`)?.focus(),i.every((e=>e&&!isNaN(+e)))&&(document.querySelector(`input[name=code-${a}]`)?.blur(),await o?.(i.join(``)))};return(0,b.jsx)(_,{title:`Enter confirmation code`,subtitle:(0,b.jsxs)(`span`,t===`email`?{children:[`Please check `,(0,b.jsx)(R,{children:e}),` for an email from`,` `,n??`privy.io`,` and enter your code below.`]}:{children:[`Please check `,(0,b.jsx)(R,{children:e}),` for a`,i?` WhatsApp`:``,` message from `,r,` and enter your code below.`]}),icon:t===`email`?m:h,onBack:a,showBack:!0,helpText:(0,b.jsxs)(I,{children:[(0,b.jsxs)(`span`,{children:[`Didn't get `,t===`email`?`an email`:`a message`,`?`]}),u?(0,b.jsxs)(L,{children:[(0,b.jsx)(C,{color:`var(--privy-color-foreground)`,strokeWidth:1.33,height:`12px`,width:`12px`}),(0,b.jsx)(`span`,{children:`Code sent`})]}):(0,b.jsx)(g,{as:`button`,size:`sm`,onClick:s,children:`Resend code`})]}),children:(0,b.jsx)(N,{children:(0,b.jsx)(y,{children:(0,b.jsxs)(P,{children:[(0,b.jsx)(`div`,{children:p.map(((e,t)=>(0,b.jsx)(`input`,{name:`code-${t}`,type:`text`,value:p[t],onChange:S,onKeyUp:e=>{e.key===`Backspace`&&(e=>{f?.(),v([...p.slice(0,e),``,...p.slice(e+1)]),e>0&&document.querySelector(`input[name=code-${e-1}]`)?.focus()})(t)},inputMode:`numeric`,autoFocus:t===0,pattern:`[0-9]`,className:`${l?`success`:``} ${c?`fail`:``}`,autoComplete:w.isMobile?`one-time-code`:`off`},t)))}),(0,b.jsx)(F,{$fail:!!c,$success:l,children:(0,b.jsx)(`span`,{children:c===`Invalid or expired verification code`?`Incorrect code`:c||(l?`Success!`:``)})})]})})})})},E=6,D=[,,,,,,].fill(``),O,k,A=((O=A||{})[O.RESET_AFTER_DELAY=0]=`RESET_AFTER_DELAY`,O[O.CLEAR_ON_NEXT_VALID_INPUT=1]=`CLEAR_ON_NEXT_VALID_INPUT`,O),j=((k=j||{})[k.EMAIL=0]=`EMAIL`,k[k.SMS=1]=`SMS`,k),M={component:()=>{let{navigate:e,lastScreen:t,navigateBack:n,setModalData:a,onUserCloseViaDialogOrKeybindRef:s}=p(),f=r(),{closePrivyModal:m,resendEmailCode:h,resendSmsCode:g,getAuthMeta:_,loginWithCode:y,updateWallets:S,createAnalyticsEvent:C}=o(),{authenticated:w,logout:E,user:D}=c(),{whatsAppEnabled:O}=r(),[k,A]=(0,x.useState)(!1),[j,M]=(0,x.useState)(null),[N,P]=(0,x.useState)(null),[F,I]=(0,x.useState)(0);s.current=()=>null;let L=+!_()?.email,R=L===0?_()?.email||``:_()?.phoneNumber||``,z=i-500;return(0,x.useEffect)((()=>{if(F){let e=setTimeout((()=>{I(F-1)}),1e3);return()=>clearTimeout(e)}}),[F]),(0,x.useEffect)((()=>{if(w&&k&&D){if(f?.legal.requireUsersAcceptTerms&&!D.hasAcceptedTerms){let t=setTimeout((()=>{e(`AffirmativeConsentScreen`)}),z);return()=>clearTimeout(t)}if(v(D,f.embeddedWallets)){let t=setTimeout((()=>{a({createWallet:{onSuccess:()=>{},onFailure:e=>{console.error(e),C({eventName:`embedded_wallet_creation_failure_logout`,payload:{error:e,screen:`AwaitingPasswordlessCodeScreen`}}),E()},callAuthOnSuccessOnClose:!0}}),e(`EmbeddedWalletOnAccountCreateScreen`)}),z);return()=>clearTimeout(t)}{S();let e=setTimeout((()=>m({shouldCallAuthOnSuccess:!0,isSuccess:!0})),i);return()=>clearTimeout(e)}}}),[w,k,D]),(0,x.useEffect)((()=>{if(j&&N===0){let e=setTimeout((()=>{M(null),P(null),document.querySelector(`input[name=code-0]`)?.focus()}),1400);return()=>clearTimeout(e)}}),[j,N]),(0,b.jsx)(T,{contactMethod:R,authFlow:L===0?`email`:`sms`,emailDomain:f?.appearance.emailDomain,appName:f?.name,whatsAppEnabled:O,onBack:()=>n(),onCodeSubmit:async n=>{try{await y(n),A(!0)}catch(n){if(n instanceof d&&n.privyErrorCode===u.INVALID_CREDENTIALS)M(`Invalid or expired verification code`),P(0);else if(n instanceof d&&n.privyErrorCode===u.CANNOT_LINK_MORE_OF_TYPE)M(n.message);else{if(n instanceof d&&n.privyErrorCode===u.USER_LIMIT_REACHED)return console.error(new l(n).toString()),void e(`UserLimitReachedScreen`);if(n instanceof d&&n.privyErrorCode===u.USER_DOES_NOT_EXIST)return void e(`AccountNotFoundScreen`);if(n instanceof d&&n.privyErrorCode===u.LINKED_TO_ANOTHER_USER)return a({errorModalData:{error:n,previousScreen:t??`AwaitingPasswordlessCodeScreen`}}),void e(`ErrorScreen`,!1);if(n instanceof d&&n.privyErrorCode===u.DISALLOWED_PLUS_EMAIL)return a({inlineError:{error:n}}),void e(`ConnectOrCreateScreen`,!1);if(n instanceof d&&n.privyErrorCode===u.ACCOUNT_TRANSFER_REQUIRED&&n.data?.data?.nonce)return a({accountTransfer:{nonce:n.data?.data?.nonce,account:R,displayName:n.data?.data?.account?.displayName,linkMethod:L===0?`email`:`sms`,embeddedWalletAddress:n.data?.data?.otherUser?.embeddedWalletAddress}}),void e(`LinkConflictScreen`);M(`Issue verifying code`),P(0)}}},onResend:async()=>{I(30),L===0?await h():await g()},errorMessage:j||void 0,success:k,resendCountdown:F,onInvalidInput:e=>{M(e),P(1)},onClearError:()=>{N===1&&(M(null),P(null))}})}},N=a.div`
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  margin: auto;
  gap: 16px;
  flex-grow: 1;
  width: 100%;
`,P=a.div`
  display: flex;
  flex-direction: column;
  width: 100%;
  gap: 12px;

  > div:first-child {
    display: flex;
    justify-content: center;
    gap: 0.5rem;
    width: 100%;
    border-radius: var(--privy-border-radius-sm);

    > input {
      border: 1px solid var(--privy-color-foreground-4);
      background: var(--privy-color-background);
      border-radius: var(--privy-border-radius-sm);
      padding: 8px 10px;
      height: 48px;
      width: 40px;
      text-align: center;
      font-size: 18px;
      font-weight: 600;
      color: var(--privy-color-foreground);
      transition: all 0.2s ease;
    }

    > input:focus {
      border: 1px solid var(--privy-color-foreground);
      box-shadow: 0 0 0 1px var(--privy-color-foreground);
    }

    > input:invalid {
      border: 1px solid var(--privy-color-error);
    }

    > input.success {
      border: 1px solid var(--privy-color-border-success);
      background: var(--privy-color-success-bg);
    }

    > input.fail {
      border: 1px solid var(--privy-color-border-error);
      background: var(--privy-color-error-bg);
      animation: shake 180ms;
      animation-iteration-count: 2;
    }
  }

  @keyframes shake {
    0% {
      transform: translate(1px, 0px);
    }
    33% {
      transform: translate(-1px, 0px);
    }
    67% {
      transform: translate(-1px, 0px);
    }
    100% {
      transform: translate(1px, 0px);
    }
  }
`,F=a.div`
  line-height: 20px;
  min-height: 20px;
  font-size: 14px;
  font-weight: 400;
  color: ${e=>e.$success?`var(--privy-color-success-dark)`:e.$fail?`var(--privy-color-error-dark)`:`transparent`};
  display: flex;
  justify-content: center;
  width: 100%;
  text-align: center;
`,I=a.div`
  display: flex;
  gap: 8px;
  align-items: center;
  justify-content: center;
  width: 100%;
  color: var(--privy-color-foreground-2);
`,L=a.div`
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: var(--privy-border-radius-sm);
  padding: 2px 8px;
  gap: 4px;
  background: var(--privy-color-background-2);
  color: var(--privy-color-foreground-2);
`,R=a.span`
  font-weight: 500;
  word-break: break-all;
  color: var(--privy-color-foreground);
`;export{M as AwaitingPasswordlessCodeScreen,M as default,T as AwaitingPasswordlessCodeScreenView};