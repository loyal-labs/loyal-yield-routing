export const THEME_STORAGE_KEY = "theme";

export function isDarkTheme(storedTheme: string | null): boolean {
  return storedTheme === "dark";
}

export function readStoredTheme(): string | null {
  try {
    return localStorage.getItem(THEME_STORAGE_KEY);
  } catch {
    return null;
  }
}

export const initialThemeScript = `try{if(localStorage.getItem(${JSON.stringify(THEME_STORAGE_KEY)})==="dark")document.documentElement.classList.add("dark")}catch(e){}`;
