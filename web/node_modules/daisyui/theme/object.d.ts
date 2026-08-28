interface Theme {
  "color-scheme": string
  "--color-base-100": string
  "--color-base-200": string
  "--color-base-300": string
  "--color-base-content": string
  "--color-primary": string
  "--color-primary-content": string
  "--color-secondary": string
  "--color-secondary-content": string
  "--color-accent": string
  "--color-accent-content": string
  "--color-neutral": string
  "--color-neutral-content": string
  "--color-info": string
  "--color-info-content": string
  "--color-success": string
  "--color-success-content": string
  "--color-warning": string
  "--color-warning-content": string
  "--color-error": string
  "--color-error-content": string
  "--radius-selector": string
  "--radius-field": string
  "--radius-box": string
  "--size-selector": string
  "--size-field": string
  "--border": string
  "--depth": string
  "--noise": string
}


interface Themes {
  dark: Theme
  autumn: Theme
  dracula: Theme
  coffee: Theme
  lemonade: Theme
  valentine: Theme
  cmyk: Theme
  emerald: Theme
  business: Theme
  aqua: Theme
  cyberpunk: Theme
  luxury: Theme
  bumblebee: Theme
  pastel: Theme
  synthwave: Theme
  wireframe: Theme
  sunset: Theme
  dim: Theme
  forest: Theme
  nord: Theme
  fantasy: Theme
  silk: Theme
  light: Theme
  black: Theme
  cupcake: Theme
  abyss: Theme
  corporate: Theme
  acid: Theme
  halloween: Theme
  garden: Theme
  retro: Theme
  caramellatte: Theme
  winter: Theme
  lofi: Theme
  night: Theme
  [key: string]: Theme
}

declare const themes: Themes
export default themes