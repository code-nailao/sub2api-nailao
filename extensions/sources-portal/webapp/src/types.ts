export interface HomeConfig {
  site_name: string;
  logo_light: string;
  logo_dark: string;
  doc_url: string;
  welfare_path: string;
  brand: {
    sections: {
      contacts: boolean;
      pricing: boolean;
      features: boolean;
      stats: boolean;
    };
    hero_badge: string;
    hero_title: string;
    hero_subtitle: string;
    primary_cta_label: string;
    primary_cta_url: string;
    secondary_cta_label: string;
    secondary_cta_url: string;
    experience_title: string;
    announcement: { enabled: boolean; text: string };
    highlights: { label: string }[];
    contacts: {
      name: string;
      account: string;
      logo: string;
      qr_image: string;
      url: string;
    }[];
    features: {
      icon: "zap" | "route" | "pie-chart" | "blocks";
      title: string;
      description: string;
    }[];
    stats: { value: string; label: string }[];
    footer_groups: { title: string; links: { label: string; url: string }[] }[];
    pricing_table: {
      headings: string[];
      rows: {
        model: string;
        spec?: string;
        group: string;
        multiplier: string;
        input: string;
        output: string;
        official: string;
        discount: string;
      }[];
    };
  };
}
