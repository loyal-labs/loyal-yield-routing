import{s as e}from"./rolldown-runtime-Bjp52BOI.js";import{t}from"./react-BPZ7Dl3u.js";import{t as n}from"./jsx-runtime-CndCZlyt.js";import{Ht as r}from"./index-wGOlH2Wi-BIV0ccqi.js";import{C as i}from"./context-DEETNFc9-CiNZw6c8.js";import{E as a,Ht as o,m as s}from"./useActiveWallet-Cid7Y-Qy-BNUGq4bX.js";import{Tn as c}from"./ccip-CDRmV2C3.js";import{t as l}from"./formatters-DJewiR9y.js";import{i as u,l as d,o as f,r as p,u as m}from"./ModalHeader-CAqKnddp-BVsHJbWY.js";import{t as h}from"./ErrorMessage-D8VaAP5m-HXKyCiFl.js";import{t as g}from"./LabelXs-oqZNqbm_-LfXPARoT.js";import{t as _}from"./Address-BNlptHuN-Bz4rV4Aa.js";import{t as v}from"./shared-FM0rljBt-BQNO2ZRw.js";import{t as y}from"./WalletInfoCard-DCGeaps8-B4u66Eh-.js";import{t as b}from"./ChevronDownIcon-t7K8N2_6.js";import{n as x,t as S}from"./Checkbox-BhNoOKjX-BUpI1-kW.js";import{t as C}from"./ExclamationCircleIcon-CBwANIin.js";import{t as ee}from"./Subtitle-CV-2yKE4-Ds90KST1.js";import{t as te}from"./Title-BnzYV3Is-D35CPcIO.js";import{a as w,i as T,n as E,r as ne,t as D}from"./Value-tcJV9e0L-CzByu5Pj.js";import{t as O}from"./LoadingSkeleton-U6-3yFwI-BN8sYcnn.js";import{t as k}from"./WarningBanner-D5LqDt95-D8R-0bXU.js";import{t as A}from"./ErrorBanner-CQERa7bL-BvoBqyEi.js";var j=e(t());function M({title:e,titleId:t,...n},r){return j.createElement(`svg`,Object.assign({xmlns:`http://www.w3.org/2000/svg`,fill:`none`,viewBox:`0 0 24 24`,strokeWidth:1.5,stroke:`currentColor`,"aria-hidden":`true`,"data-slot":`icon`,ref:r,"aria-labelledby":t},n),e?j.createElement(`title`,{id:t},e):null,j.createElement(`path`,{strokeLinecap:`round`,strokeLinejoin:`round`,d:`M13.5 4.5 21 12m0 0-7.5 7.5M21 12H3`}))}var re=j.forwardRef(M);function N({title:e,titleId:t,...n},r){return j.createElement(`svg`,Object.assign({xmlns:`http://www.w3.org/2000/svg`,fill:`none`,viewBox:`0 0 24 24`,strokeWidth:1.5,stroke:`currentColor`,"aria-hidden":`true`,"data-slot":`icon`,ref:r,"aria-labelledby":t},n),e?j.createElement(`title`,{id:t},e):null,j.createElement(`path`,{strokeLinecap:`round`,strokeLinejoin:`round`,d:`m3.75 13.5 10.5-11.25L12 10.5h8.25L9.75 21.75 12 13.5H3.75Z`}))}var ie=j.forwardRef(N),P=n();function F({title:e,titleId:t,...n},r){return j.createElement(`svg`,Object.assign({xmlns:`http://www.w3.org/2000/svg`,fill:`none`,viewBox:`0 0 24 24`,strokeWidth:1.5,stroke:`currentColor`,"aria-hidden":`true`,"data-slot":`icon`,ref:r,"aria-labelledby":t},n),e?j.createElement(`title`,{id:t},e):null,j.createElement(`path`,{strokeLinecap:`round`,strokeLinejoin:`round`,d:`M8.25 7.5V6.108c0-1.135.845-2.098 1.976-2.192.373-.03.748-.057 1.123-.08M15.75 18H18a2.25 2.25 0 0 0 2.25-2.25V6.108c0-1.135-.845-2.098-1.976-2.192a48.424 48.424 0 0 0-1.123-.08M15.75 18.75v-1.875a3.375 3.375 0 0 0-3.375-3.375h-1.5a1.125 1.125 0 0 1-1.125-1.125v-1.5A3.375 3.375 0 0 0 6.375 7.5H5.25m11.9-3.664A2.251 2.251 0 0 0 15 2.25h-1.5a2.251 2.251 0 0 0-2.15 1.586m5.8 0c.065.21.1.433.1.664v.75h-6V4.5c0-.231.035-.454.1-.664M6.75 7.5H4.875c-.621 0-1.125.504-1.125 1.125v12c0 .621.504 1.125 1.125 1.125h9.75c.621 0 1.125-.504 1.125-1.125V16.5a9 9 0 0 0-9-9Z`}))}var I=j.forwardRef(F),ae=a(D)`
  cursor: pointer;
  display: inline-flex;
  gap: 8px;
  align-items: center;
  color: var(--privy-color-accent);
  svg {
    fill: var(--privy-color-accent);
  }
`,L=({iconUrl:e,value:t,symbol:n,usdValue:r,nftName:i,nftCount:a,decimals:o,$isLoading:s})=>{if(s)return(0,P.jsx)(R,{$isLoading:s});let c=t&&r&&o?function(e,t,n){let r=parseFloat(e),i=parseFloat(n);if(r===0||i===0||Number.isNaN(r)||Number.isNaN(i))return e;let a=Math.ceil(-Math.log10(.01/(i/r))),o=10**(a=Math.max(a=Math.min(a,t),1)),s=+(Math.floor(r*o)/o).toFixed(a).replace(/\.?0+$/,``);return Intl.NumberFormat(void 0,{maximumFractionDigits:t}).format(s)}(t,o,r):t;return(0,P.jsxs)(`div`,{children:[(0,P.jsxs)(R,{$isLoading:s,children:[e&&(0,P.jsx)(B,{src:e,alt:`Token icon`}),a&&a>1?a+`x`:void 0,` `,i,c,` `,n]}),r&&(0,P.jsxs)(z,{$isLoading:s,children:[`$`,r]})]})},R=a.span`
  color: var(--privy-color-foreground);
  font-size: 0.875rem;
  font-weight: 500;
  line-height: 1.375rem;
  word-break: break-all;
  text-align: right;
  display: flex;
  justify-content: flex-end;

  ${O}
`,z=a.span`
  color: var(--privy-color-foreground-2);
  font-size: 12px;
  font-weight: 400;
  line-height: 18px;
  word-break: break-all;
  text-align: right;
  display: flex;
  justify-content: flex-end;

  ${O}
`,B=a.img`
  height: 14px;
  width: 14px;
  margin-right: 4px;
  object-fit: contain;
`,oe=e=>{let{chain:t,transactionDetails:n,isTokenContractInfoLoading:r,symbol:i}=e,{action:a,functionName:o}=n;return(0,P.jsx)(v,{children:(0,P.jsxs)(w,{children:[a!==`transaction`&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Action`}),(0,P.jsx)(E,{children:o})]}),o===`mint`&&`args`in n&&n.args.filter((e=>e)).map(((e,n)=>(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Param ${n}`}),(0,P.jsx)(E,{children:typeof e==`string`&&c(e)?(0,P.jsx)(_,{address:e,url:t?.blockExplorers?.default?.url,showCopyIcon:!1}):e?.toString()})]},n))),o===`setApprovalForAll`&&n.operator&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Operator`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:n.operator,url:t?.blockExplorers?.default?.url,showCopyIcon:!1})})]}),o===`setApprovalForAll`&&n.approved!==void 0&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Set approval to`}),(0,P.jsx)(E,{children:n.approved?`true`:`false`})]}),o===`transfer`||o===`transferWithMemo`||o===`transferFrom`||o===`safeTransferFrom`||o===`approve`?(0,P.jsxs)(P.Fragment,{children:[`formattedAmount`in n&&n.formattedAmount&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Amount`}),(0,P.jsxs)(E,{$isLoading:r,children:[n.formattedAmount,` `,i]})]}),`tokenId`in n&&n.tokenId&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token ID`}),(0,P.jsx)(E,{children:n.tokenId.toString()})]})]}):null,o===`safeBatchTransferFrom`&&(0,P.jsxs)(P.Fragment,{children:[`amounts`in n&&n.amounts&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Amounts`}),(0,P.jsx)(E,{children:n.amounts.join(`, `)})]}),`tokenIds`in n&&n.tokenIds&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token IDs`}),(0,P.jsx)(E,{children:n.tokenIds.join(`, `)})]})]}),o===`approve`&&n.spender&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Spender`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:n.spender,url:t?.blockExplorers?.default?.url,showCopyIcon:!1})})]}),(o===`transferFrom`||o===`safeTransferFrom`||o===`safeBatchTransferFrom`)&&n.transferFrom&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Transferring from`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:n.transferFrom,url:t?.blockExplorers?.default?.url,showCopyIcon:!1})})]}),(o===`transferFrom`||o===`safeTransferFrom`||o===`safeBatchTransferFrom`)&&n.transferTo&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Transferring to`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:n.transferTo,url:t?.blockExplorers?.default?.url,showCopyIcon:!1})})]})]})})},se=({variant:e,setPreventMaliciousTransaction:t,colorScheme:n=`light`,preventMaliciousTransaction:r})=>e===`warn`?(0,P.jsx)(V,{children:(0,P.jsxs)(k,{theme:n,children:[(0,P.jsx)(`span`,{style:{fontWeight:`500`},children:`Warning: Suspicious transaction`}),(0,P.jsx)(`br`,{}),`This has been flagged as a potentially deceptive request. Approving could put your assets or funds at risk.`]})}):e===`error`?(0,P.jsx)(P.Fragment,{children:(0,P.jsxs)(V,{children:[(0,P.jsx)(A,{theme:n,children:(0,P.jsxs)(`div`,{children:[(0,P.jsx)(`strong`,{children:`This is a malicious transaction`}),(0,P.jsx)(`br`,{}),`This transaction transfers tokens to a known malicious address. Proceeding may result in the loss of valuable assets.`]})}),(0,P.jsxs)(ce,{children:[(0,P.jsx)(S,{color:`var(--privy-color-error)`,checked:!r,readOnly:!0,onClick:()=>t(!r)}),(0,P.jsx)(`span`,{children:`I understand and want to proceed anyways.`})]})]})}):null,V=a.div`
  margin-top: 1.5rem;
`,ce=a.div`
  display: flex;
  align-items: center;
  gap: 0.5rem;
  margin-top: 0.75rem;
`,le=({transactionIndex:e,maxIndex:t})=>typeof e!=`number`||t===0?``:` (${e+1} / ${t+1})`,H=({img:e,submitError:t,prepareError:n,onClose:r,action:a,title:o,subtitle:c,to:l,tokenAddress:f,network:g,missingFunds:v,fee:y,from:x,cta:S,disabled:C,chain:O,isSubmitting:k,isPreparing:A,isTokenPriceLoading:M,isTokenContractInfoLoading:N,isSponsored:F,symbol:I,balance:R,onClick:z,transactionDetails:B,transactionIndex:V,maxIndex:ce,onBack:H,chainName:U,validation:G,hasScanDetails:K,setIsScanDetailsOpen:me,preventMaliciousTransaction:he,setPreventMaliciousTransaction:ge,tokensSent:q,tokensReceived:J,isScanning:_e,isCancellable:ve,functionName:Y})=>{let{showTransactionDetails:X,setShowTransactionDetails:Z,hasMoreDetails:Q,isErc20Ish:ye}=(e=>{let[t,n]=(0,j.useState)(!1),r=!0,i=!1;return(!e||e.isErc20Ish||e.action===`transaction`)&&(r=!1),r&&(i=Object.entries(e||{}).some((([e,t])=>t&&![`action`,`isErc20Ish`,`isNFTIsh`].includes(e)))),{showTransactionDetails:t,setShowTransactionDetails:n,hasMoreDetails:r&&i,isErc20Ish:e?.isErc20Ish}})(B),$=i(),be=ye&&N||A||M||_e;return(0,P.jsxs)(P.Fragment,{children:[(0,P.jsx)(p,{onClose:r,backFn:H}),e&&(0,P.jsx)(de,{children:e}),(0,P.jsxs)(te,{style:{marginTop:e?`1.5rem`:0},children:[o,(0,P.jsx)(le,{maxIndex:ce,transactionIndex:V})]}),(0,P.jsx)(ee,{children:c}),(0,P.jsxs)(w,{style:{marginTop:`2rem`},children:[(!!q[0]||be)&&(0,P.jsxs)(T,{children:[J.length>0?(0,P.jsx)(D,{children:`Send`}):(0,P.jsx)(D,{children:a===`approve`?`Approval amount`:`Amount`}),(0,P.jsx)(`div`,{className:`flex flex-col`,children:q.map(((e,t)=>(0,P.jsx)(L,{iconUrl:e.iconUrl,value:Y===`setApprovalForAll`?`All`:e.value,usdValue:e.usdValue,symbol:e.symbol,nftName:e.nftName,nftCount:e.nftCount,decimals:e.decimals},t)))})]}),J.length>0&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Receive`}),(0,P.jsx)(`div`,{className:`flex flex-col`,children:J.map(((e,t)=>(0,P.jsx)(L,{iconUrl:e.iconUrl,value:e.value,usdValue:e.usdValue,symbol:e.symbol,nftName:e.nftName,nftCount:e.nftCount,decimals:e.decimals},t)))})]}),B&&`spender`in B&&B?.spender?(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Spender`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:B.spender,url:O?.blockExplorers?.default?.url})})]}):null,l&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`To`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:l,url:O?.blockExplorers?.default?.url,showCopyIcon:!0})})]}),f&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token address`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:f,url:O?.blockExplorers?.default?.url})})]}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Network`}),(0,P.jsx)(E,{children:g})]}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Estimated fee`}),(0,P.jsx)(E,{$isLoading:A||M||F===void 0,children:F?(0,P.jsxs)(fe,{children:[(0,P.jsxs)(pe,{children:[`Sponsored by `,$.name]}),(0,P.jsx)(ie,{height:16,width:16})]}):y})]}),Q&&!K&&(0,P.jsxs)(P.Fragment,{children:[(0,P.jsx)(T,{className:`cursor-pointer`,onClick:()=>Z(!X),children:(0,P.jsxs)(ne,{className:`flex items-center gap-x-1`,children:[`Details`,` `,(0,P.jsx)(b,{style:{width:`0.75rem`,marginLeft:`0.25rem`,transform:X?`rotate(180deg)`:void 0}})]})}),X&&B&&(0,P.jsx)(oe,{action:a,chain:O,transactionDetails:B,isTokenContractInfoLoading:N,symbol:I})]}),K&&(0,P.jsx)(T,{children:(0,P.jsxs)(ae,{onClick:()=>me(!0),children:[(0,P.jsx)(`span`,{className:`text-color-primary`,children:`Details`}),(0,P.jsx)(re,{height:`14px`,width:`14px`,strokeWidth:`2`})]})})]}),(0,P.jsx)(s,{}),t?(0,P.jsx)(h,{style:{marginTop:`2rem`},children:t.message}):n&&V===0?(0,P.jsx)(h,{style:{marginTop:`2rem`},children:n.shortMessage??ue}):null,(0,P.jsx)(se,{variant:G,preventMaliciousTransaction:he,setPreventMaliciousTransaction:ge}),(0,P.jsx)(W,{$useSmallMargins:!(!n&&!t&&G!==`warn`&&G!==`error`),address:x,balance:R,errMsg:A||n||t||!v?void 0:`Add funds on ${O?.name??U} to complete transaction.`}),(0,P.jsx)(d,{style:{marginTop:`1rem`},loading:k,disabled:C||A,onClick:z,children:S}),ve&&(0,P.jsx)(u,{style:{marginTop:`1rem`},onClick:r,isSubmitting:!1,children:`Not now`}),(0,P.jsx)(m,{})]})},U=({img:e,title:t,subtitle:n,cta:r,instructions:a,network:o,blockExplorerUrl:c,isMissingFunds:u,submitError:f,parseError:v,total:y,swap:x,transactingWalletAddress:S,fee:C,balance:ne,disabled:O,isSubmitting:k,isPreparing:A,isTokenPriceLoading:M,onClick:re,onClose:N,onBack:F,isSponsored:I})=>{let L=A||M,[R,z]=(0,j.useState)(!1),B=i();return(0,P.jsxs)(P.Fragment,{children:[(0,P.jsx)(p,{onClose:N,backFn:F}),e&&(0,P.jsx)(de,{children:e}),(0,P.jsx)(te,{style:{marginTop:e?`1.5rem`:0},children:t}),(0,P.jsx)(ee,{children:n}),(0,P.jsxs)(w,{style:{marginTop:`2rem`,marginBottom:`.5rem`},children:[(y||L)&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Amount`}),(0,P.jsx)(E,{$isLoading:L,children:y})]}),x&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Swap`}),(0,P.jsx)(E,{children:x})]}),o&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Network`}),(0,P.jsx)(E,{children:o})]}),(C||L||I!==void 0)&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Estimated fee`}),(0,P.jsx)(E,{$isLoading:L,children:I&&!L?(0,P.jsxs)(fe,{children:[(0,P.jsxs)(pe,{children:[`Sponsored by `,B.name]}),(0,P.jsx)(ie,{height:16,width:16})]}):C})]})]}),(0,P.jsx)(T,{children:(0,P.jsxs)(ae,{onClick:()=>z((e=>!e)),children:[(0,P.jsx)(`span`,{children:`Advanced`}),(0,P.jsx)(b,{height:`16px`,width:`16px`,strokeWidth:`2`,style:{transition:`all 300ms`,transform:R?`rotate(180deg)`:void 0}})]})}),R&&(0,P.jsx)(P.Fragment,{children:a.map(((e,t)=>e.type===`sol-transfer`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsxs)(g,{children:[`Transfer `,e.withSeed?`with seed`:``]})}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Amount`}),(0,P.jsxs)(E,{children:[l({amount:e.value,decimals:e.token.decimals}),` `,e.token.symbol]})]}),!!e.toAccount&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Destination`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.toAccount,url:c})})]})]},t):e.type===`spl-transfer`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsxs)(g,{children:[`Transfer `,e.token.symbol]})}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Amount`}),(0,P.jsx)(E,{children:e.value.toString()})]}),!!e.fromAta&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Source`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.fromAta,url:c})})]}),!!e.toAta&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Destination`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.toAta,url:c})})]}),!!e.token.address&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.token.address,url:c})})]})]},t):e.type===`ata-creation`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsx)(g,{children:`Create token account`})}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Program ID`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.program,url:c})})]}),!!e.owner&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Owner`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.owner,url:c})})]})]},t):e.type===`create-account`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsxs)(g,{children:[`Create account `,e.withSeed?`with seed`:``]})}),!!e.account&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Account`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.account,url:c})})]}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Amount`}),(0,P.jsxs)(E,{children:[l({amount:e.value,decimals:9}),` SOL`]})]})]},t):e.type===`spl-init-account`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsx)(g,{children:`Initialize token account`})}),!!e.account&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Account`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.account,url:c})})]}),!!e.mint&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Mint`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.mint,url:c})})]}),!!e.owner&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Owner`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.owner,url:c})})]})]},t):e.type===`spl-close-account`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsx)(g,{children:`Close token account`})}),!!e.source&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Source`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.source,url:c})})]}),!!e.destination&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Destination`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.destination,url:c})})]}),!!e.owner&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Owner`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.owner,url:c})})]})]},t):e.type===`spl-sync-native`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsx)(g,{children:`Sync native`})}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Program ID`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.program,url:c})})]})]},t):e.type===`raydium-swap-base-input`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsxs)(g,{children:[`Raydium swap`,` `,e.tokenIn&&e.tokenOut?`${e.tokenIn.symbol} → ${e.tokenOut.symbol}`:``]})}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Amount in`}),(0,P.jsx)(E,{children:e.amountIn.toString()})]}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Minimum amount out`}),(0,P.jsx)(E,{children:e.minimumAmountOut.toString()})]}),e.mintIn&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token in`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.mintIn,url:c})})]}),e.mintOut&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token out`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.mintOut,url:c})})]})]},t):e.type===`raydium-swap-base-output`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsxs)(g,{children:[`Raydium swap`,` `,e.tokenIn&&e.tokenOut?`${e.tokenIn.symbol} → ${e.tokenOut.symbol}`:``]})}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Max amount in`}),(0,P.jsx)(E,{children:e.maxAmountIn.toString()})]}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Amount out`}),(0,P.jsx)(E,{children:e.amountOut.toString()})]}),e.mintIn&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token in`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.mintIn,url:c})})]}),e.mintOut&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token out`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.mintOut,url:c})})]})]},t):e.type===`jupiter-swap-shared-accounts-route`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsxs)(g,{children:[`Jupiter swap`,` `,e.tokenIn&&e.tokenOut?`${e.tokenIn.symbol} → ${e.tokenOut.symbol}`:``]})}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`In amount`}),(0,P.jsx)(E,{children:e.inAmount.toString()})]}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Quoted out amount`}),(0,P.jsx)(E,{children:e.quotedOutAmount.toString()})]}),e.mintIn&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token in`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.mintIn,url:c})})]}),e.mintOut&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token out`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.mintOut,url:c})})]})]},t):e.type===`jupiter-swap-exact-out-route`?(0,P.jsxs)(G,{children:[(0,P.jsx)(T,{children:(0,P.jsxs)(g,{children:[`Jupiter swap`,` `,e.tokenIn&&e.tokenOut?`${e.tokenIn.symbol} → ${e.tokenOut.symbol}`:``]})}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Quoted in amount`}),(0,P.jsx)(E,{children:e.quotedInAmount.toString()})]}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Amount out`}),(0,P.jsx)(E,{children:e.outAmount.toString()})]}),e.mintIn&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token in`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.mintIn,url:c})})]}),e.mintOut&&(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Token out`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.mintOut,url:c})})]})]},t):(0,P.jsxs)(G,{children:[(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Program ID`}),(0,P.jsx)(E,{children:(0,P.jsx)(_,{address:e.program,url:c})})]}),(0,P.jsxs)(T,{children:[(0,P.jsx)(D,{children:`Data`}),(0,P.jsx)(E,{children:e.discriminator})]})]},t)))}),(0,P.jsx)(s,{}),f?(0,P.jsx)(h,{style:{marginTop:`2rem`},children:f.message}):v?(0,P.jsx)(h,{style:{marginTop:`2rem`},children:ue}):null,(0,P.jsx)(W,{$useSmallMargins:!(!v&&!f),title:``,address:S,balance:ne,errMsg:A||v||f||!u?void 0:`Add funds on Solana to complete transaction.`}),(0,P.jsx)(d,{style:{marginTop:`1rem`},loading:k,disabled:O||A,onClick:re,children:r}),(0,P.jsx)(m,{})]})},W=a(y)`
  ${e=>e.$useSmallMargins?`margin-top: 0.5rem;`:`margin-top: 2rem;`}
`,G=a(w)`
  margin-top: 0.5rem;
  border: 1px solid var(--privy-color-foreground-4);
  border-radius: var(--privy-border-radius-sm);
  padding: 0.5rem;
`,ue=`There was an error preparing your transaction. Your transaction request will likely fail.`,de=a.div`
  display: flex;
  width: 100%;
  justify-content: center;
  max-height: 40px;

  > img {
    object-fit: contain;
    border-radius: var(--privy-border-radius-sm);
  }
`,fe=a.span`
  display: inline-flex;
  align-items: center;
  gap: 0.3rem;
`,pe=a.span`
  font-size: 14px;
  font-weight: 500;
  color: var(--privy-color-foreground);
`,K=e=>e?.code===r.COMPLIANCE_BLOCKED,me=()=>(0,P.jsxs)(_e,{children:[(0,P.jsx)(Y,{}),(0,P.jsx)(ve,{})]}),he=({transactionError:e,chainId:t,onClose:n,onRetry:r,chainType:i,transactionHash:a})=>{let{chains:s}=o(),[c,l]=(0,j.useState)(!1),{errorCode:u,errorMessage:d}=((e,t)=>{if(t===`ethereum`)return K(e)?{errorCode:`Transaction blocked`,errorMessage:e.message}:{errorCode:e.details??e.message,errorMessage:e.shortMessage};let n=e.txSignature,r=e?.transactionMessage||`Something went wrong.`;if(Array.isArray(e.logs)){let t=e.logs.find((e=>/insufficient (lamports|funds)/gi.test(e)));t&&(r=t)}return{transactionHash:n,errorMessage:r}})(e,i),m=K(e),h=(({chains:e,chainId:t,chainType:n,transactionHash:r})=>n===`ethereum`?e.find((e=>e.id===t))?.blockExplorers?.default.url??`https://etherscan.io`:function(e,t){return`https://explorer.solana.com/tx/${e}?chain=${t}`}(r||``,t))({chains:s,chainId:t,chainType:i,transactionHash:a});return(0,P.jsxs)(P.Fragment,{children:[(0,P.jsx)(p,{onClose:n}),(0,P.jsxs)(ge,{children:[(0,P.jsx)(me,{}),(0,P.jsx)(q,{children:u}),(0,P.jsx)(J,{children:m?`This transaction cannot be completed.`:`Please try again.`}),(0,P.jsxs)(Q,{children:[(0,P.jsx)(Z,{children:`Error message`}),(0,P.jsx)($,{$clickable:!1,children:d})]}),a&&(0,P.jsxs)(Q,{children:[(0,P.jsx)(Z,{children:`Transaction hash`}),(0,P.jsxs)(ye,{children:[`Copy this hash to view details about the transaction on a`,` `,(0,P.jsx)(`u`,{children:(0,P.jsx)(`a`,{href:h,children:`block explorer`})}),`.`]}),(0,P.jsxs)($,{$clickable:!0,onClick:async()=>{await navigator.clipboard.writeText(a),l(!0)},children:[a,(0,P.jsx)(Se,{clicked:c})]})]}),!m&&(0,P.jsx)(X,{onClick:()=>r({resetNonce:!!a}),children:`Retry transaction`})]}),(0,P.jsx)(f,{})]})},ge=a.div`
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
`,q=a.span`
  color: var(--privy-color-foreground);
  text-align: center;
  font-size: 1.125rem;
  font-weight: 500;
  line-height: 1.25rem; /* 111.111% */
  text-align: center;
  margin: 10px;
`,J=a.span`
  margin-top: 4px;
  margin-bottom: 10px;
  color: var(--privy-color-foreground-3);
  text-align: center;

  font-size: 0.875rem;
  font-style: normal;
  font-weight: 400;
  line-height: 20px; /* 142.857% */
  letter-spacing: -0.008px;
`,_e=a.div`
  position: relative;
  width: 60px;
  height: 60px;
  margin: 10px;
  display: flex;
  justify-content: center;
  align-items: center;
`,ve=a(C)`
  position: absolute;
  width: 35px;
  height: 35px;
  color: var(--privy-color-error);
`,Y=a.div`
  position: absolute;
  width: 60px;
  height: 60px;
  border-radius: 50%;
  background-color: var(--privy-color-error);
  opacity: 0.1;
`,X=a(d)`
  && {
    margin-top: 24px;
  }
  transition:
    color 350ms ease,
    background-color 350ms ease;
`,Z=a.span`
  width: 100%;
  text-align: left;
  font-size: 0.825rem;
  color: var(--privy-color-foreground);
  padding: 4px;
`,Q=a.div`
  width: 100%;
  margin: 5px;
  display: flex;
  flex-direction: column;
  justify-content: center;
  align-items: center;
`,ye=a.text`
  position: relative;
  width: 100%;
  padding: 5px;
  font-size: 0.8rem;
  color: var(--privy-color-foreground-3);
  text-align: left;
  word-wrap: break-word;
`,$=a.span`
  position: relative;
  width: 100%;
  background-color: var(--privy-color-background-2);
  padding: 8px 12px;
  border-radius: 10px;
  margin-top: 5px;
  font-size: 14px;
  color: var(--privy-color-foreground-3);
  text-align: left;
  word-wrap: break-word;
  ${e=>e.$clickable&&`cursor: pointer;
  transition: background-color 0.3s;
  padding-right: 45px;

  &:hover {
    background-color: var(--privy-color-foreground-4);
  }`}
`,be=a(I)`
  position: absolute;
  top: 13px;
  right: 13px;
  width: 24px;
  height: 24px;
`,xe=a(x)`
  position: absolute;
  top: 13px;
  right: 13px;
  width: 24px;
  height: 24px;
`,Se=({clicked:e})=>(0,P.jsx)(e?xe:be,{});export{U as n,he as r,H as t};