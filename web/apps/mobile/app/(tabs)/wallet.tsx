import { Redirect, useLocalSearchParams } from "expo-router";
export default function WalletRedirect() {
  const params = useLocalSearchParams();
  return <Redirect href={{ pathname: "/(tabs)", params }} />;
}
