import Image from "next/image"
import { useSearchParams } from "next/navigation"
import { LoginForm } from "@/components/login-form"

export default function LoginPage() {
  const params = useSearchParams()
  const next = params.get("next")
  const nextPath = next === "/" ? "/earn" : next?.startsWith("/") ? next : undefined

  return (
    <div className="bg-muted flex min-h-svh flex-col items-center justify-center gap-6 p-6 md:p-10">
      <div className="flex w-full max-w-sm flex-col gap-6">
        <Image
          src="/sidebar-brand.svg"
          alt="Loyal"
          width={140}
          height={28}
          className="h-7 w-auto self-center"
          priority
        />
        <LoginForm hasError={params.get("error") === "invalid"} nextPath={nextPath} />
      </div>
    </div>
  )
}
