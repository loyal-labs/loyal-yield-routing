import{t as e}from"./jsx-runtime-CndCZlyt.js";import{C as t,E as n,T as r}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{t as i}from"./LoadingSkeleton-U6-3yFwI-BN8sYcnn.js";var a=e(),o=({children:e,color:t,isLoading:n,isPulsing:r,...i})=>(0,a.jsx)(s,{$color:t,$isLoading:n,$isPulsing:r,...i,children:e}),s=n.span`
  padding: 0.25rem;
  font-size: 0.75rem;
  font-weight: 500;
  line-height: 1rem; /* 150% */
  border-radius: var(--privy-border-radius-xs);
  display: flex;
  align-items: center;
  ${e=>{let n,i;e.$color===`green`&&(n=`var(--privy-color-success-dark)`,i=`var(--privy-color-success-light)`),e.$color===`red`&&(n=`var(--privy-color-error)`,i=`var(--privy-color-error-light)`),e.$color===`gray`&&(n=`var(--privy-color-foreground-2)`,i=`var(--privy-color-background-2)`);let a=t`
      from, to {
        background-color: ${i};
      }

      50% {
        background-color: rgba(${i}, 0.8);
      }
    `;return r`
      color: ${n};
      background-color: ${i};
      ${e.$isPulsing&&r`
        animation: ${a} 3s linear infinite;
      `};
    `}}

  ${i}
`;export{o as t};