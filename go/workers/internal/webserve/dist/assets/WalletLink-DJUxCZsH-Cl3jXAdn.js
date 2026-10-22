import{t as e}from"./jsx-runtime-CndCZlyt.js";import{E as t,N as n}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{g as r}from"./_esm-CDZwTn6X.js";import{i,n as a,o,t as s}from"./ethers-By_elEXl-D1yEwqwg.js";import{t as c}from"./getFormattedUsdFromLamports-B6EqSEho-CWXnni59.js";import{n as l}from"./transaction-CnfuREWo-D7B2_ywf.js";var u=e(),d=({weiQuantities:e,tokenPrice:t,tokenSymbol:n})=>{let r=i(e),a=t?o(r,t):void 0,c=s(r,n);return(0,u.jsx)(m,{children:a||c})},f=({weiQuantities:e,tokenPrice:t,tokenSymbol:n})=>{let r=i(e),a=t?o(r,t):void 0,c=s(r,n);return(0,u.jsx)(m,{children:a?(0,u.jsxs)(u.Fragment,{children:[(0,u.jsx)(h,{children:`USD`}),a===`<$0.01`?(0,u.jsxs)(_,{children:[(0,u.jsx)(g,{children:`<`}),`$0.01`]}):a]}):c})},p=({quantities:e,tokenPrice:t,tokenSymbol:n=`SOL`,tokenDecimals:i=9})=>{let a=e.reduce(((e,t)=>e+t),0n),o=t&&n===`SOL`&&i===9?c(a,t):void 0,s=n===`SOL`&&i===9?l(a):`${r(a,i)} ${n}`;return(0,u.jsx)(m,{children:o?(0,u.jsx)(u.Fragment,{children:o===`<$0.01`?(0,u.jsxs)(_,{children:[(0,u.jsx)(g,{children:`<`}),`$0.01`]}):o}):s})},m=t.span`
  font-size: 14px;
  line-height: 140%;
  display: flex;
  gap: 4px;
  align-items: center;
`,h=t.span`
  font-size: 12px;
  line-height: 12px;
  color: var(--privy-color-foreground-3);
`,g=t.span`
  font-size: 10px;
`,_=t.span`
  display: flex;
  align-items: center;
`;function v(e,t){return`https://explorer.solana.com/account/${e}?chain=${t}`}var y=e=>(0,u.jsx)(b,{href:e.chainType===`ethereum`?a(e.chainId,e.walletAddress):v(e.walletAddress,e.chainId),target:`_blank`,children:n(e.walletAddress)}),b=t.a`
  &:hover {
    text-decoration: underline;
  }
`;export{d as i,p as n,f as r,y as t};