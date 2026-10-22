import{s as e}from"./rolldown-runtime-Bjp52BOI.js";import{t}from"./react-BPZ7Dl3u.js";import{t as n}from"./jsx-runtime-CndCZlyt.js";import{E as r}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{t as i}from"./check-CMoNoGI-.js";import{t as a}from"./copy-5KFdobpP.js";import{t as o}from"./ModalHeader-CAqKnddp-BVsHJbWY.js";import{t as s}from"./ErrorMessage-D8VaAP5m-HXKyCiFl.js";import{t as c}from"./LabelXs-oqZNqbm_-LfXPARoT.js";import{t as l}from"./Address-BNlptHuN-Bz4rV4Aa.js";import{t as u}from"./shared-FM0rljBt-BQNO2ZRw.js";var d=n(),f=e(t(),1),p=r(u)`
  && {
    padding: 0.75rem;
    height: 56px;
  }
`,m=r.div`
  display: flex;
  align-items: center;
  justify-content: space-between;
  width: 100%;
`,h=r.div`
  display: flex;
  flex-direction: column;
  gap: 0;
`,g=r.div`
  font-size: 12px;
  line-height: 1rem;
  color: var(--privy-color-foreground-3);
`,_=r(c)`
  text-align: left;
  margin-bottom: 0.5rem;
`,v=r(s)`
  margin-top: 0.25rem;
`,y=r(o)`
  && {
    gap: 0.375rem;
    font-size: 14px;
  }
`,b=({errMsg:e,balance:t,address:n,className:r,title:o,showCopyButton:s=!1})=>{let[c,u]=(0,f.useState)(!1);return(0,f.useEffect)((()=>{if(c){let e=setTimeout((()=>u(!1)),3e3);return()=>clearTimeout(e)}}),[c]),(0,d.jsxs)(`div`,{children:[o&&(0,d.jsx)(_,{children:o}),(0,d.jsx)(p,{className:r,$state:e?`error`:void 0,children:(0,d.jsxs)(m,{children:[(0,d.jsxs)(h,{children:[(0,d.jsx)(l,{address:n,showCopyIcon:!1}),t!==void 0&&(0,d.jsx)(g,{children:t})]}),s&&(0,d.jsx)(y,{onClick:function(e){e.stopPropagation(),navigator.clipboard.writeText(n).then((()=>u(!0))).catch(console.error)},size:`sm`,children:(0,d.jsxs)(d.Fragment,c?{children:[`Copied`,(0,d.jsx)(i,{size:14})]}:{children:[`Copy`,(0,d.jsx)(a,{size:14})]})})]})}),e&&(0,d.jsx)(v,{children:e})]})};export{b as t};