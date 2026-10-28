import * as Clipboard from "expo-clipboard";
import { StatusBar } from "expo-status-bar";
import { useIsFocused } from "@react-navigation/native";
import { Alert, Image, ScrollView, StyleSheet } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { TAB_BAR_HEIGHT } from "@/components/TabBar";
import { Pressable, Text, View } from "@/tw";

import Autodeposit from "../../../assets/images/quests/autodeposit.svg";
import EarnCoin from "../../../assets/images/quests/earn-coin.svg";
import BoneBalance from "../../../assets/images/quests/bone-balance.svg";
import BoneClaim from "../../../assets/images/quests/bone-claim.svg";
import BoneCooldown from "../../../assets/images/quests/bone-cooldown.svg";
import BoneEarned from "../../../assets/images/quests/bone-earned.svg";
import BoneRank from "../../../assets/images/quests/bone-rank.svg";
import BoneReward from "../../../assets/images/quests/bone-reward.svg";
import Check from "../../../assets/images/quests/check.svg";
import Chevron from "../../../assets/images/quests/chevron.svg";
import Copy from "../../../assets/images/quests/copy.svg";
import Crown from "../../../assets/images/quests/crown.svg";
import Email from "../../../assets/images/quests/email.svg";
import HeaderHelp from "../../../assets/images/quests/header-help.svg";
import Help from "../../../assets/images/quests/help.svg";
import Jupiter from "../../../assets/images/quests/jupiter.svg";
import Lock from "../../../assets/images/quests/lock.svg";
import MultiplierIcon from "../../../assets/images/quests/multiplier.svg";
import RankMultiplier from "../../../assets/images/quests/rank-multiplier.svg";
import RankTip from "../../../assets/images/quests/rank-tip.svg";
import Streak from "../../../assets/images/quests/streak.svg";
import Telegram from "../../../assets/images/quests/telegram.svg";
import X from "../../../assets/images/quests/x.svg";

const LOYAL_ICON = require("../../../assets/images/quests/loyal-token.png");

// Presentation fixtures, matching the web preview; no rewards or eligibility writes.
const QUESTS = [
  { title: "Follow Loyal on X", icon: "x", state: "claim" },
  { title: "Join Telegram community", icon: "telegram", state: "available" },
  { title: "Buy $LOYAL on Jupiter", icon: "jupiter", state: "available" },
  {
    title: "Deposit at least $100 into Earn",
    icon: "earn",
    state: "available",
  },
  { title: "Set up Autodeposit", icon: "autodeposit", state: "locked" },
  {
    title: "Link your email to your account",
    icon: "email",
    state: "completed",
  },
] as const;

function showPreviewNotice(title = "Quests are coming soon") {
  Alert.alert(
    title,
    "Rewards and invite codes aren’t available yet. Check back soon.",
    [{ text: "Got it" }]
  );
}

function Amount({
  value,
  large = false,
  small = false,
}: {
  value: string;
  large?: boolean;
  small?: boolean;
}) {
  const [whole, fraction] = value.split(".");
  return (
    <Text
      style={[
        styles.amount,
        large && styles.balance,
        small && styles.smallAmount,
      ]}
    >
      {whole}
      <Text style={styles.tertiary}>.{fraction}</Text>
    </Text>
  );
}

function Multiplier({ value }: { value: string }) {
  return (
    <View style={styles.multiplier}>
      <MultiplierIcon />
      <Text style={styles.multiplierText}>×{value}</Text>
    </View>
  );
}

function QuestIcon({ name }: { name: (typeof QUESTS)[number]["icon"] }) {
  switch (name) {
    case "x":
      return <X />;
    case "telegram":
      return <Telegram />;
    case "jupiter":
      return <Jupiter />;
    case "email":
      return (
        <View style={styles.emailIcon}>
          <Email />
        </View>
      );
    case "earn":
      return (
        <View style={styles.emailIcon}>
          <View style={styles.earnCoin}>
            <EarnCoin />
          </View>
        </View>
      );
    case "autodeposit":
      return <Autodeposit />;
  }
}

function EarningCard({
  kind,
  inProgress,
}: {
  kind: "earn" | "loyal";
  inProgress: boolean;
}) {
  const isEarn = kind === "earn";
  const cooldown = inProgress && !isEarn;
  const title = isEarn ? "Keep cash in Earn" : "Hold $LOYAL";
  return (
    <View style={[styles.card, cooldown && styles.cooldownCard]}>
      <View style={styles.cardHeader}>
        {isEarn ? (
          <QuestIcon name="earn" />
        ) : (
          <Image source={LOYAL_ICON} style={styles.icon} />
        )}
        <Text style={styles.cardTitle}>{title}</Text>
        <Pressable
          accessibilityRole="button"
          accessibilityLabel={`About ${title}`}
          hitSlop={12}
          onPress={() =>
            Alert.alert(
              title,
              isEarn
                ? "Keep cash in Earn to collect Bones. Your streak grows while you keep your deposit. This preview shows sample rewards."
                : "Hold $LOYAL to collect Bones. Your streak grows while you keep your tokens. This preview shows sample rewards."
            )
          }
        >
          <Help />
        </Pressable>
      </View>
      <View style={styles.cardStats}>
        <View style={styles.total}>
          <Text style={styles.caption}>
            {inProgress ? "Total earned" : "Bones earned"}
          </Text>
          <View style={styles.inline}>
            <Amount value={inProgress ? "1,248.56" : "0.00"} />
            <BoneEarned />
          </View>
        </View>
        <View style={styles.statsRow}>
          <View style={styles.stat}>
            <Text style={styles.caption}>
              {isEarn ? "Earn balance" : "$LOYAL held"}
            </Text>
            <Amount value={inProgress ? "$1,248.56" : "$0.00"} small />
            <Multiplier value={inProgress && isEarn ? "1.25" : "0.25"} />
          </View>
          <View style={styles.stat}>
            <Text style={styles.caption}>Streak</Text>
            <View style={styles.inline}>
              <Streak />
              <Text style={styles.medium}>{inProgress ? "7D" : "0D"}</Text>
            </View>
            <Multiplier value={inProgress ? "1.25" : "1.00"} />
          </View>
        </View>
      </View>
      {cooldown && (
        <Text style={styles.cooldownNote}>
          {
            "Withdrawing liquidity will reset Bones\nto zero for the current claiming epoch"
          }
        </Text>
      )}
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={
          inProgress
            ? `${title}: ${
                cooldown ? "Claim in 16h 59m" : "Claim"
              } 250.45 Bones`
            : isEarn
            ? "Deposit to Earn"
            : "Get $LOYAL"
        }
        accessibilityState={{ disabled: cooldown }}
        disabled={cooldown}
        onPress={() => showPreviewNotice()}
        style={({ pressed }) => [
          styles.claimButton,
          (!inProgress || cooldown) && styles.mutedClaim,
          pressed && styles.pressed,
        ]}
      >
        <Text
          style={[styles.claimText, (!inProgress || cooldown) && styles.brand]}
        >
          {inProgress
            ? cooldown
              ? "Claim in 16h 59m"
              : "Claim"
            : isEarn
            ? "Deposit to Earn"
            : "Get $LOYAL"}
        </Text>
        {inProgress && (
          <View style={styles.inline}>
            <Text style={[styles.claimAmount, cooldown && styles.brand]}>
              +250.45
            </Text>
            {cooldown ? <BoneCooldown /> : <BoneClaim />}
          </View>
        )}
      </Pressable>
    </View>
  );
}

export function QuestsScreen({ inProgress = true }: { inProgress?: boolean }) {
  const insets = useSafeAreaInsets();
  const focused = useIsFocused();
  const copyReferral = async () => {
    try {
      const copied = await Clipboard.setStringAsync("BXP68Q");
      Alert.alert(
        copied ? "Referral code copied" : "Could not copy referral code"
      );
    } catch {
      Alert.alert("Could not copy referral code");
    }
  };
  return (
    <View style={[styles.screen, { paddingTop: insets.top }]}>
      {focused && <StatusBar style="dark" />}
      <ScrollView
        showsVerticalScrollIndicator={false}
        contentContainerStyle={[
          styles.content,
          { paddingBottom: TAB_BAR_HEIGHT + Math.max(insets.bottom, 12) + 16 },
        ]}
      >
        <View style={styles.header}>
          <Text accessibilityRole="header" style={styles.heading}>
            Quests
          </Text>
          <Pressable
            accessibilityRole="button"
            accessibilityLabel="About Quests"
            onPress={() =>
              Alert.alert(
                "Quests",
                "Collect Bones by completing quests, keeping cash in Earn, and holding $LOYAL. Build a streak and progress through ranks. This preview uses sample balances; rewards aren’t available yet."
              )
            }
          >
            <HeaderHelp />
          </Pressable>
        </View>
        <View style={styles.summary}>
          <View style={styles.rank}>
            <Crown />
            <Text style={styles.rankText}>{inProgress ? "Scout" : "Pup"}</Text>
            <View style={styles.rankMultiplier}>
              <RankMultiplier />
              <Text style={styles.rankMultiplierText}>
                ×{inProgress ? "1.25" : "0.25"}
              </Text>
            </View>
          </View>
          <View style={styles.inline}>
            <Amount value={inProgress ? "11,250.62" : "0.00"} large />
            <BoneBalance />
          </View>
          <View
            accessibilityRole="progressbar"
            accessibilityLabel="Progress to next rank"
            accessibilityValue={{ min: 0, max: 100, now: inProgress ? 29 : 0 }}
            style={styles.progress}
          >
            <View
              style={[styles.progressFill, { width: inProgress ? "29%" : 0 }]}
            />
          </View>
          <View style={styles.nextRank}>
            <View style={styles.tip}>
              <RankTip />
            </View>
            <View style={styles.nextRankBadge}>
              <Text style={styles.nextRankText}>
                {inProgress ? "9,260" : "10,000"}
              </Text>
              <BoneRank />
              <Text style={styles.nextRankText}>
                to {inProgress ? "Guardian" : "Scout"}
              </Text>
            </View>
          </View>
          <View style={styles.referrals}>
            <Pressable
              accessibilityRole="button"
              accessibilityLabel="Copy referral code BXP68Q"
              onPress={copyReferral}
              style={({ pressed }) => [
                styles.referral,
                pressed && styles.pressed,
              ]}
            >
              <Text style={styles.caption}>Referral code</Text>
              <View style={styles.inline}>
                <Text style={styles.medium}>BXP68Q</Text>
                <Copy />
              </View>
            </Pressable>
            <Pressable
              accessibilityRole="button"
              onPress={() => showPreviewNotice("Have an invite code?")}
              style={({ pressed }) => [
                styles.invite,
                pressed && styles.pressed,
              ]}
            >
              <Text style={styles.inviteText}>I have an invite code</Text>
            </Pressable>
          </View>
        </View>
        <View style={styles.cards}>
          <EarningCard kind="earn" inProgress={inProgress} />
          <EarningCard kind="loyal" inProgress={inProgress} />
        </View>
        <View style={styles.quests}>
          {QUESTS.map((quest) => {
            const disabled =
              quest.state === "locked" || quest.state === "completed";
            return (
              <Pressable
                key={quest.title}
                accessibilityRole="button"
                accessibilityLabel={`${quest.title}, ${quest.state}, 250 Bones`}
                accessibilityState={{ disabled }}
                disabled={disabled}
                onPress={() => showPreviewNotice()}
                style={({ pressed }) => [
                  styles.quest,
                  quest.state === "completed" && styles.completed,
                  pressed && styles.pressed,
                ]}
              >
                <QuestIcon name={quest.icon} />
                <View style={styles.questCopy}>
                  <Text style={styles.questTitle}>{quest.title}</Text>
                  <View style={styles.inline}>
                    <Text style={styles.reward}>+250</Text>
                    <BoneReward />
                  </View>
                </View>
                {quest.state === "claim" ? (
                  <View style={styles.smallClaim}>
                    <Text style={styles.smallClaimText}>Claim</Text>
                  </View>
                ) : (
                  <View style={styles.questState}>
                    {quest.state === "locked" ? (
                      <Lock />
                    ) : quest.state === "completed" ? (
                      <Check />
                    ) : (
                      <Chevron />
                    )}
                  </View>
                )}
              </Pressable>
            );
          })}
        </View>
      </ScrollView>
    </View>
  );
}

const BRAND = "#F9363C";
const TERTIARY = "#AFADB3";
const SECONDARY = "#8A8A8E";
const SURFACE = "rgba(0, 0, 0, 0.04)";
const REGULAR = "Geist_400Regular";
const MEDIUM = "Geist_500Medium";
const SEMIBOLD = "Geist_600SemiBold";

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: "#FFF" },
  content: { width: "100%", maxWidth: 768, alignSelf: "center" },
  header: {
    flexDirection: "row",
    alignItems: "center",
    paddingLeft: 16,
    paddingRight: 8,
    paddingTop: 16,
    paddingBottom: 8,
  },
  heading: {
    flex: 1,
    fontFamily: SEMIBOLD,
    fontSize: 24,
    lineHeight: 28,
    color: "#000",
    paddingVertical: 8,
  },
  summary: { paddingHorizontal: 16, paddingTop: 8, gap: 8 },
  inline: { flexDirection: "row", alignItems: "center", gap: 4 },
  amount: {
    fontFamily: SEMIBOLD,
    fontSize: 28,
    lineHeight: 32,
    color: "#000",
    fontVariant: ["tabular-nums"],
  },
  balance: { fontSize: 32, lineHeight: 36, letterSpacing: -0.352 },
  smallAmount: { fontFamily: MEDIUM, fontSize: 17, lineHeight: 22 },
  tertiary: { color: TERTIARY },
  brand: { color: BRAND },
  rank: {
    flexDirection: "row",
    gap: 6,
    alignItems: "center",
    alignSelf: "flex-start",
    backgroundColor: TERTIARY,
    borderRadius: 56,
    paddingLeft: 8,
    paddingRight: 4,
    paddingVertical: 3,
  },
  rankText: { fontFamily: MEDIUM, fontSize: 17, lineHeight: 22, color: "#FFF" },
  rankMultiplier: {
    flexDirection: "row",
    gap: 2,
    alignItems: "center",
    backgroundColor: "#FFF",
    borderRadius: 25,
    paddingHorizontal: 6,
    paddingVertical: 2,
  },
  rankMultiplierText: {
    fontFamily: MEDIUM,
    fontSize: 13,
    lineHeight: 16,
    color: TERTIARY,
  },
  progress: {
    height: 16,
    borderRadius: 20,
    backgroundColor: SURFACE,
    marginTop: 16,
    overflow: "hidden",
  },
  progressFill: { height: 16, borderRadius: 16, backgroundColor: BRAND },
  nextRank: { alignSelf: "flex-end", marginTop: -4 },
  tip: { alignItems: "flex-end", paddingRight: 12 },
  nextRankBadge: {
    flexDirection: "row",
    alignItems: "center",
    gap: 4,
    backgroundColor: TERTIARY,
    borderRadius: 56,
    paddingHorizontal: 8,
    paddingVertical: 4,
  },
  nextRankText: {
    fontFamily: REGULAR,
    fontSize: 13,
    lineHeight: 16,
    color: "#FFF",
  },
  referrals: { flexDirection: "row", gap: 10, marginTop: 8 },
  referral: {
    justifyContent: "center",
    paddingHorizontal: 16,
    paddingVertical: 9,
    borderRadius: 16,
    backgroundColor: SURFACE,
  },
  invite: {
    flex: 1,
    minHeight: 60,
    paddingHorizontal: 12,
    paddingVertical: 10,
    justifyContent: "center",
    borderRadius: 16,
    backgroundColor: SURFACE,
  },
  inviteText: {
    fontFamily: MEDIUM,
    fontSize: 15,
    lineHeight: 20,
    color: "#000",
    textAlign: "center",
  },
  cards: { paddingHorizontal: 16, paddingTop: 8, gap: 8 },
  card: { padding: 16, borderRadius: 24, backgroundColor: SURFACE },
  cooldownCard: { minHeight: 360 },
  cardHeader: { flexDirection: "row", alignItems: "flex-start", gap: 12 },
  icon: { width: 44, height: 44, borderRadius: 11 },
  cardTitle: {
    flex: 1,
    alignSelf: "center",
    fontFamily: REGULAR,
    fontSize: 17,
    lineHeight: 22,
    color: "#000",
  },
  cardStats: { paddingVertical: 16, flexGrow: 1 },
  total: { paddingVertical: 8, gap: 4 },
  caption: {
    fontFamily: REGULAR,
    fontSize: 15,
    lineHeight: 20,
    color: SECONDARY,
  },
  statsRow: { flexDirection: "row", gap: 8 },
  stat: { flex: 1, paddingVertical: 8, gap: 4, alignItems: "flex-start" },
  medium: { fontFamily: MEDIUM, fontSize: 17, lineHeight: 22, color: "#000" },
  multiplier: {
    flexDirection: "row",
    alignItems: "center",
    gap: 2,
    backgroundColor: SURFACE,
    borderRadius: 56,
    paddingHorizontal: 6,
    paddingVertical: 2,
  },
  multiplierText: {
    fontFamily: REGULAR,
    fontSize: 13,
    lineHeight: 16,
    color: SECONDARY,
  },
  cooldownNote: {
    fontFamily: REGULAR,
    fontSize: 11,
    lineHeight: 13,
    color: SECONDARY,
    textAlign: "center",
    paddingBottom: 12,
  },
  claimButton: {
    minHeight: 48,
    flexDirection: "row",
    alignItems: "center",
    alignContent: "center",
    justifyContent: "space-between",
    flexWrap: "wrap",
    gap: 4,
    backgroundColor: BRAND,
    borderRadius: 40,
    paddingHorizontal: 16,
    paddingVertical: 10,
  },
  mutedClaim: { backgroundColor: "rgba(249, 54, 60, 0.14)" },
  claimText: {
    fontFamily: MEDIUM,
    fontSize: 17,
    lineHeight: 22,
    includeFontPadding: false,
    textAlignVertical: "center",
    color: "#FFF",
  },
  claimAmount: {
    fontFamily: SEMIBOLD,
    fontSize: 17,
    lineHeight: 22,
    includeFontPadding: false,
    textAlignVertical: "center",
    color: "#FFF",
    fontVariant: ["tabular-nums"],
  },
  quests: { paddingTop: 24 },
  quest: {
    minHeight: 60,
    flexDirection: "row",
    alignItems: "center",
    gap: 12,
    paddingHorizontal: 16,
    paddingVertical: 8,
    borderRadius: 16,
  },
  questCopy: { flex: 1, gap: 2 },
  questTitle: {
    fontFamily: MEDIUM,
    fontSize: 17,
    lineHeight: 22,
    letterSpacing: -0.187,
    color: "#000",
  },
  reward: { fontFamily: MEDIUM, fontSize: 15, lineHeight: 20, color: BRAND },
  smallClaim: {
    backgroundColor: BRAND,
    borderRadius: 40,
    minWidth: 64,
    paddingHorizontal: 16,
    paddingVertical: 8,
  },
  smallClaimText: {
    fontFamily: MEDIUM,
    fontSize: 15,
    lineHeight: 20,
    color: "#FFF",
    textAlign: "center",
  },
  questState: { width: 24, alignItems: "center", justifyContent: "center" },
  earnCoin: { position: "absolute", left: 12.83, top: 6.42 },
  emailIcon: {
    width: 44,
    height: 44,
    borderRadius: 11,
    backgroundColor: BRAND,
    alignItems: "center",
    justifyContent: "center",
  },
  completed: { opacity: 0.4 },
  pressed: { opacity: 0.7 },
});
