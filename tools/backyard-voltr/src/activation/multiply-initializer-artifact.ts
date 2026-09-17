/** Reviewed seed-149..151 rollout artifact. Installed authority is verified separately. */
import { createHash } from "node:crypto";
import { generated } from "@loyal-labs/loyal-smart-accounts-core";
import { type PublicKey } from "@solana/web3.js";
import { assertPolicyMatchesArtifact, derivePolicyAddress, policyExpectationFromArtifact, type BasicPolicyRow } from "./rwa-basic-policy-set.js";

export const INITIALIZER_ARTIFACT_SHA256 = "97da2aa6cb44bfd5bdabb58d064efbf0d7c9646cf9602eb8094c80bb22626d33";
export const INITIALIZER_SETTINGS = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
export const INITIALIZER_ADMIN = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
export const INITIALIZER_PROGRAM = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG";
export const INITIALIZER_DELEGATE = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const sha = (bytes:Uint8Array) => createHash("sha256").update(bytes).digest("hex");
export type InitializerArtifact = { policySeedBefore:string; policies:readonly BasicPolicyRow[] };

export function readInitializerArtifact(bytes:Buffer): InitializerArtifact {
 if (sha(bytes)!==INITIALIZER_ARTIFACT_SHA256) throw new Error("Initializer artifact differs from the reviewed compiler output; refresh and review before changing authority");
 const artifact=JSON.parse(bytes.toString("utf8"));
 if(artifact.schema!=="loyal-backyard-multiply-initializer-install/v1"||artifact.settings!==INITIALIZER_SETTINGS||artifact.authority!==INITIALIZER_ADMIN||artifact.delegate!==INITIALIZER_DELEGATE||artifact.policySeedBefore!=="148"||artifact.policies.length!==3)throw new Error("Initializer artifact identity mismatch");
 const policies=artifact.policies.map((row:BasicPolicyRow & {lane:string},index:number)=>{
  if(row.seed!==String(149+index)||row.account!==derivePolicyAddress(INITIALIZER_SETTINGS,row.seed))throw new Error("Initializer policy seed mismatch");
  const policy:BasicPolicyRow={...row,family:row.lane,dataSha256:sha(Buffer.from(row.instruction.dataBase64,"base64")),constraints:[]};
  const expected=policyExpectationFromArtifact(policy);
  if(expected.settings!==INITIALIZER_SETTINGS||expected.seed!==row.seed||expected.accountIndex!==0||expected.threshold!==1||expected.timeLock!==0||expected.signers.length!==1||expected.signers[0]?.key!==INITIALIZER_DELEGATE||expected.signers[0]?.permissionsMask!==7||expected.constraints.length!==1)throw new Error("Initializer policy authority mismatch");
  return {...policy,constraints:expected.constraints};
 });
 return {policySeedBefore:artifact.policySeedBefore,policies};
}

type PolicyState = Parameters<typeof assertPolicyMatchesArtifact>[1];
const Policy=(generated as unknown as {Policy:{deserialize(data:Buffer):readonly [PolicyState,number]}}).Policy;
export function verifyInitializerPolicy(policy:BasicPolicyRow,account:{owner:string|PublicKey;executable:boolean;data:Buffer;lamports:number},blockTime?:number):string {
 if(String(account.owner)!==INITIALIZER_PROGRAM||account.executable||account.lamports<=0)throw new Error("Initializer policy account identity mismatch");
 const [decoded,offset]=Policy.deserialize(account.data);
 if(account.data.length!==934||offset!==576||account.data.subarray(offset).some(byte=>byte!==0))throw new Error("Initializer policy allocation or padding mismatch");
 assertPolicyMatchesArtifact(policy,decoded,blockTime===undefined?undefined:{blockTime});
 return sha(account.data);
}
