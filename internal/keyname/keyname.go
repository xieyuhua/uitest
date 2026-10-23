// Package keyname 把中文业务标签转成可读的英文字段名（data_key）。
//
// 为什么需要：插件从 label/placeholder/name 推导 data_key 时，纯中文会退化成 field_25
// 这种名字，AI 生成数据、日志、断言都没法读。这里用「业务词典 + 贪心最长匹配」把它变成
// remember_password / product_name / price 这种一眼能懂的键。
//
// 设计取舍：不引入拼音库（体积大、离线不可得）。常见业务词走词典；词典覆盖不到的单字
// 用常用汉字拼音兜底；再不行就丢弃该字，最后仍然为空时由调用方回退到 field_N。
package keyname

import (
	"strings"
	"unicode"
)

// compounds 业务词 → 英文（按"最长优先"匹配，多字词写全，避免被单字拆碎）
var compounds = map[string]string{
	// 登录 / 账户
	"记住密码": "remember_password", "忘记密码": "forgot_password", "确认密码": "confirm_password",
	"登录密码": "login_password", "新密码": "new_password", "旧密码": "old_password", "密码": "password",
	"登录名": "login_name", "登录账号": "login_account", "账号": "account", "账户": "account",
	"用户名": "username", "昵称": "nickname", "验证码": "captcha", "图形验证码": "captcha",
	"手机号": "mobile", "手机": "mobile", "电话": "phone", "联系方式": "contact", "邮箱": "email",
	"身份证": "id_card", "姓名": "full_name", "真实姓名": "real_name",

	// 商品 / 订单
	"商品名称": "product_name", "商品标题": "product_title", "商品编号": "product_code",
	"商品分类": "category", "商品类型": "product_type", "商品描述": "description",
	"服务名称": "service_name", "服务商品": "service_product", "名称": "name", "标题": "title",
	"价格": "price", "单价": "unit_price", "售价": "sale_price", "原价": "original_price",
	"金额": "amount", "总价": "total_price", "成本价": "cost_price", "折扣": "discount",
	"库存": "stock", "库存数量": "stock", "数量": "quantity", "件数": "quantity", "重量": "weight",
	"规格": "spec", "单位": "unit", "品牌": "brand", "型号": "model", "条码": "barcode",
	"编码": "code", "编号": "code", "排序": "sort", "序号": "seq", "sku": "sku",
	"上架": "on_shelf", "下架": "off_shelf", "状态": "status", "是否上架": "is_on_shelf",

	// 订单 / 客户
	"订单号": "order_no", "订单编号": "order_no", "订单状态": "order_status", "订单": "order",
	"客户名称": "customer_name", "客户": "customer", "会员": "member", "等级": "level",
	"供应商": "supplier", "仓库": "warehouse", "物流": "logistics", "快递单号": "tracking_no",
	"收货人": "receiver", "收货地址": "shipping_address", "地址": "address", "区域": "region",
	"省": "province", "市": "city", "区县": "district", "街道": "street", "邮编": "zipcode",

	// 活动 / 营销
	"活动名称": "activity_name", "活动类型": "activity_type", "活动": "activity",
	"开始时间": "start_time", "结束时间": "end_time", "开始日期": "start_date",
	"有效期": "validity", "生效时间": "effective_time", "过期时间": "expire_time",
	"优惠券": "coupon", "满减": "full_reduction", "秒杀": "seckill", "拼团": "group_buy",
	"佣金": "commission", "税率": "tax_rate", "运费": "shipping_fee", "积分": "points",

	// 内容 / 媒体
	"图片": "image", "封面": "cover", "封面图片": "cover_image", "头像": "avatar", "图标": "icon",
	"附件": "attachment", "视频": "video", "文件": "file", "上传": "upload",
	"描述": "description", "备注": "remark", "说明": "remark", "内容": "content",
	"详情": "detail", "简介": "intro", "摘要": "summary", "关键词": "keywords", "标签": "tags",
	"链接": "url", "网址": "url", "地址链接": "url", "搜索": "search", "关键字": "keyword",

	// 时间
	"时间": "time", "日期": "date", "创建时间": "created_at", "更新时间": "updated_at",
	"生日": "birthday", "年龄": "age", "性别": "gender", "部门": "department", "职位": "position",
	"工号": "employee_no", "入职时间": "hire_date", "离职时间": "leave_date", "薪资": "salary",

	// 通用动作/前缀
	"请选择": "", "请输入": "", "请填写": "", "选择": "", "输入": "",
	"是否": "is_", "启用": "enabled", "禁用": "disabled", "开关": "switch",
	"同意": "agree", "记住": "remember",
}

// singles 常用汉字拼音兜底（词典没覆盖到时的最后手段）
var singles = map[rune]string{
	'名': "name", '称': "name", '价': "price", '格': "price", '数': "num", '量': "qty",
	'库': "stock", '存': "stock", '分': "cate", '类': "type", '型': "type", '描': "desc",
	'述': "desc", '备': "remark", '注': "note", '密': "pwd", '码': "code", '记': "remember",
	'住': "remember", '手': "mobile", '机': "phone", '电': "phone", '话': "phone",
	'邮': "mail", '箱': "box", '地': "addr", '址': "addr", '时': "time", '间': "time",
	'日': "date", '期': "date", '状': "status", '态': "status", '账': "account", '号': "no",
	'用': "user", '户': "user", '订': "order", '单': "order", '商': "product", '品': "product",
	'活': "activity", '动': "activity", '折': "discount", '扣': "discount", '图': "image",
	'片': "image", '封': "cover", '面': "page", '标': "tag", '题': "title", '内': "content",
	'容': "content", '编': "code", '位': "unit", '规': "spec", '重': "weight", '开': "start",
	'始': "start", '结': "end", '束': "end", '是': "is", '否': "not", '搜': "search",
	'索': "search", '登': "login", '录': "login", '册': "reg", '姓': "surname",
	'公': "company", '司': "company", '店': "shop", '铺': "shop", '牌': "brand", '供': "supply",
	'应': "supply", '仓': "warehouse", '客': "customer", '消': "msg", '息': "msg",
	'验': "verify", '证': "verify", '链': "url", '接': "url", '排': "sort", '序': "sort",
	'佣': "commission", '金': "money", '税': "tax", '运': "shipping", '费': "fee",
	'省': "province", '市': "city", '区': "district", '街': "street", '道': "street",
	'门': "gate", '银': "bank", '行': "bank", '卡': "card", '性': "gender", '别': "gender",
	'年': "year", '月': "month", '天': "day", '小': "mini", '秒': "second", '部': "dept",
	'职': "position", '工': "job", '入': "in", '离': "leave", '薪': "salary", '资': "salary",
	'奖': "bonus", '保': "warranty", '修': "repair", '有': "has", '效': "valid", '限': "limit",
	'说': "desc", '明': "desc", '简': "simple", '介': "intro", '详': "detail", '情': "detail",
	'列': "list", '表': "table", '推': "recommend", '荐': "recommend", '热': "hot",
	'新': "new", '房': "warehouse", '物': "logistics", '流': "logistics", '快': "express",
	'递': "express", '发': "send", '货': "goods", '收': "receive", '退': "refund",
	'款': "refund", '售': "sale", '后': "after", '评': "review", '级': "level", '星': "star",
}

// asciiWord 记录连续的字母/数字，原样保留（如 sku、v2）
func asciiWord(sb *strings.Builder, r rune) {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		sb.WriteRune(unicode.ToLower(r))
	}
}

// DeriveKey 把中文标签转成 snake_case 字段名；无法转换时返回空串。
//
// 例：记住密码 → remember_password；商品名称 → product_name；价格（元）→ price；SKU编码 → sku_code
func DeriveKey(label, placeholder, name string) string {
	for _, cand := range []string{label, placeholder, name} {
		if k := convert(cand); k != "" {
			return k
		}
	}
	return ""
}

func convert(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	var sb strings.Builder
	lastUnderscore := true
	appendPart := func(part string) {
		part = strings.Trim(part, "_")
		if part == "" {
			return
		}
		if !lastUnderscore {
			sb.WriteRune('_')
		}
		sb.WriteString(part)
		lastUnderscore = false
	}

	for i := 0; i < len(runes); {
		// 1) 词典：贪心最长匹配（最多 6 个字）
		matched := false
		for n := 6; n >= 2; n-- {
			if i+n > len(runes) {
				continue
			}
			if en, ok := compounds[string(runes[i:i+n])]; ok {
				if en != "" {
					appendPart(en)
				}
				i += n
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		// 2) 单字词典
		r := runes[i]
		if en, ok := compounds[string(r)]; ok {
			if en != "" {
				appendPart(en)
			}
			i++
			continue
		}
		if en, ok := singles[r]; ok {
			appendPart(en)
			i++
			continue
		}
		// 3) 字母/数字原样保留
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			var tmp strings.Builder
			asciiWord(&tmp, r)
			// 连续 ASCII 直接拼，不加下划线
			sb.WriteString(tmp.String())
			lastUnderscore = false
			i++
			continue
		}
		// 4) 其它标点/符号：当作分隔
		if r < 128 || unicode.IsPunct(r) || unicode.IsSpace(r) {
			lastUnderscore = true
			i++
			continue
		}
		// 未收录的汉字：跳过
		i++
	}

	out := strings.Trim(sb.String(), "_")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	if len(out) > 40 {
		out = strings.Trim(out[:40], "_")
	}
	// 纯数字的键没有意义
	if out == "" || strings.Trim(out, "0123456789_") == "" {
		return ""
	}
	return out
}
